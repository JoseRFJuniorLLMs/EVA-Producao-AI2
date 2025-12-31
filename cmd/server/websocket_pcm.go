package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"eva-mind/internal/config"
	"eva-mind/internal/database"
	"eva-mind/internal/gemini"
	"eva-mind/internal/push"

	"github.com/gorilla/websocket"
)

type PCMWebSocketHandler struct {
	upgrader    websocket.Upgrader
	clients     map[string]*PCMClient
	mu          sync.RWMutex
	cfg         *config.Config
	pushService *push.FirebaseService
	db          *database.DB
}

type PCMClient struct {
	Conn         *websocket.Conn
	CPF          string
	IdosoID      int64
	GeminiClient *gemini.Client
	SendCh       chan []byte
	mu           sync.Mutex
	active       bool
	ctx          context.Context
	cancel       context.CancelFunc
}

func NewPCMWebSocketHandler(cfg *config.Config, pushService *push.FirebaseService, db *database.DB) *PCMWebSocketHandler {
	return &PCMWebSocketHandler{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			ReadBufferSize:  8192,
			WriteBufferSize: 8192,
		},
		clients:     make(map[string]*PCMClient),
		cfg:         cfg,
		pushService: pushService,
		db:          db,
	}
}

func (h *PCMWebSocketHandler) HandlePCMConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("❌ Erro no upgrade WebSocket: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	client := &PCMClient{
		Conn:   conn,
		SendCh: make(chan []byte, 256),
		ctx:    ctx,
		cancel: cancel,
	}

	go h.handleClientSend(client)
	h.handleClientMessages(client)
}

func (h *PCMWebSocketHandler) handleClientMessages(client *PCMClient) {
	defer func() {
		client.cancel()

		if client.CPF != "" {
			h.mu.Lock()
			delete(h.clients, client.CPF)
			h.mu.Unlock()
		}

		if client.GeminiClient != nil {
			client.GeminiClient.Close()
		}

		// Evitar fechar channel duas vezes
		client.mu.Lock()
		if client.SendCh != nil {
			// close(client.SendCh) // Removido para evitar panic em concorrência, o cancel cuida do loop
		}
		client.mu.Unlock()

		client.Conn.Close()
		log.Printf("🔌 Sessão finalizada para o cliente")
	}()

	client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	client.Conn.SetPongHandler(func(string) error {
		client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		messageType, message, err := client.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("❌ Erro de leitura: %v", err)
			}
			break
		}

		client.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		if messageType == websocket.TextMessage {
			var msg map[string]interface{}
			if err := json.Unmarshal(message, &msg); err != nil {
				log.Printf("⚠️ Erro unmarshal JSON: %v", err)
				continue
			}

			msgType, _ := msg["type"].(string)

			switch msgType {
			case "register":
				h.handleRegister(client, msg)
			case "hangup":
				log.Printf("📞 Cliente solicitou desligamento")
				return
			}
		}

		if messageType == websocket.BinaryMessage {
			if !client.active || client.GeminiClient == nil {
				continue
			}

			if err := client.GeminiClient.SendAudio(message); err != nil {
				log.Printf("❌ Erro ao enviar áudio para Gemini: %v", err)
			}
		}
	}
}

func (h *PCMWebSocketHandler) handleClientSend(client *PCMClient) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-client.ctx.Done():
			return

		case audioData, ok := <-client.SendCh:
			if !ok {
				return
			}

			client.mu.Lock()
			err := client.Conn.WriteMessage(websocket.BinaryMessage, audioData)
			client.mu.Unlock()

			if err != nil {
				log.Printf("❌ Erro ao enviar áudio binário: %v", err)
				return
			}

		case <-ticker.C:
			client.mu.Lock()
			err := client.Conn.WriteMessage(websocket.PingMessage, nil)
			client.mu.Unlock()

			if err != nil {
				return
			}
		}
	}
}

func (h *PCMWebSocketHandler) handleRegister(client *PCMClient, msg map[string]interface{}) {
	cpf, ok := msg["cpf"].(string)
	if !ok || cpf == "" {
		h.sendError(client, "CPF inválido")
		return
	}

	// 1. CHAMA O MÉTODO NOVO (que deve ter a lógica de regexp_replace)
	idoso, err := h.db.GetIdosoByCPF(cpf)
	if err != nil {
		log.Printf("❌ CPF não encontrado no banco: %s", cpf)
		h.sendError(client, "CPF não autorizado ou não cadastrado")
		return
	}

	client.CPF = idoso.CPF
	client.IdosoID = idoso.ID

	h.mu.Lock()
	if existingClient, exists := h.clients[idoso.CPF]; exists {
		log.Printf("♻️ Substituindo conexão existente para o CPF: %s", idoso.CPF)
		existingClient.cancel()
		existingClient.Conn.Close()
	}
	h.clients[idoso.CPF] = client
	h.mu.Unlock()

	log.Printf("✅ Cliente autenticado: %s (ID: %d)", idoso.Nome, idoso.ID)

	// ============================================================================
	// NOVA LÓGICA: Marcar que o idoso atendeu a chamada (WATCHDOG)
	// ============================================================================
	go func() {
		_, err := h.db.GetConnection().Exec(`
			UPDATE agendamentos 
			SET status = 'em_chamada', data_hora_realizada = NOW()
			WHERE idoso_id = $1 
			  AND status IN ('agendado', 'em_andamento', 'aguardando_atendimento')
			  AND data_hora_agendada >= NOW() - INTERVAL '10 minutes'
		`, idoso.ID)

		if err != nil {
			log.Printf("❌ Erro ao atualizar status para 'em_chamada': %v", err)
		} else {
			log.Printf("📞 Idoso %d atendeu a chamada. Status atualizado.", idoso.ID)
		}
	}()
	// ============================================================================

	sessionID := fmt.Sprintf("pcm-session-%d", time.Now().UnixNano())

	// Inicializa o cliente Gemini (Multimodal Live API)
	geminiClient, err := gemini.NewClient(client.ctx, h.cfg)
	if err != nil {
		log.Printf("❌ Erro Gemini NewClient: %v", err)
		h.sendError(client, "Erro na engine de IA")
		return
	}

	client.GeminiClient = geminiClient

	// CONSTRUÇÃO DO PROMPT DINÂMICO
	instructions, err := h.buildInstructionsFromDB(client.IdosoID)
	if err != nil {
		log.Printf("⚠️ Falha ao montar prompt customizado: %v. Usando fallback.", err)
		instructions = `You are EVA, a voice assistant for elderly people in Brazil.
Speak naturally in Brazilian Portuguese. Be warm, patient, and empathetic.
Keep responses very short (1-2 sentences).`
	}

	tools := gemini.GetDefaultTools()

	// Envia o Setup para a API do Gemini
	if err := client.GeminiClient.SendSetup(instructions, tools); err != nil {
		log.Printf("❌ Erro no SendSetup do Gemini: %v", err)
		geminiClient.Close()
		h.sendError(client, "Falha na configuração da sessão de voz")
		return
	}

	// Inicia a escuta das respostas da IA em background
	go h.listenGeminiResponses(client)

	client.active = true

	response := map[string]interface{}{
		"type":      "session_created",
		"sessionId": sessionID,
		"message":   "Sessão criada! EVA está pronta para conversar.",
	}

	h.sendJSON(client, response)
}

func (h *PCMWebSocketHandler) buildInstructionsFromDB(idosoID int64) (string, error) {
	// 1. Buscar detalhes do perfil do idoso
	query := `
		SELECT 
			nome, 
			EXTRACT(YEAR FROM AGE(data_nascimento)) as idade,
			nivel_cognitivo, 
			limitacoes_auditivas, 
			usa_aparelho_auditivo, 
			tom_voz
		FROM idosos 
		WHERE id = $1
	`

	var nome, nivelCognitivo, tomVoz string
	var idade int
	var limitacoesAuditivas, usaAparelhoAuditivo bool

	err := h.db.GetConnection().QueryRow(query, idosoID).Scan(
		&nome,
		&idade,
		&nivelCognitivo,
		&limitacoesAuditivas,
		&usaAparelhoAuditivo,
		&tomVoz,
	)

	if err != nil {
		return "", fmt.Errorf("erro query idoso: %v", err)
	}

	// 2. Buscar o template de prompt base
	templateQuery := `
		SELECT template, variaveis_esperadas
		FROM prompt_templates
		WHERE nome = 'eva_base_v2' AND ativo = true
		LIMIT 1
	`

	var template string
	var variaveis string

	err = h.db.GetConnection().QueryRow(templateQuery).Scan(&template, &variaveis)
	if err != nil {
		return "", fmt.Errorf("erro query template: %v", err)
	}

	// 3. Processamento manual das variáveis do template (Lógica de Substituição)
	instructions := strings.ReplaceAll(template, "{{nome_idoso}}", nome)
	instructions = strings.ReplaceAll(instructions, "{{idade}}", fmt.Sprintf("%d", idade))
	instructions = strings.ReplaceAll(instructions, "{{nivel_cognitivo}}", nivelCognitivo)
	instructions = strings.ReplaceAll(instructions, "{{tom_voz}}", tomVoz)

	// Lógica para Limitações Auditivas
	if limitacoesAuditivas {
		instructions = strings.ReplaceAll(instructions, "{{#limitacoes_auditivas}}", "")
		instructions = strings.ReplaceAll(instructions, "{{/limitacoes_auditivas}}", "")
	} else {
		start := strings.Index(instructions, "{{#limitacoes_auditivas}}")
		end := strings.Index(instructions, "{{/limitacoes_auditivas}}")
		if start != -1 && end != -1 {
			instructions = instructions[:start] + instructions[end+len("{{/limitacoes_auditivas}}"):]
		}
	}

	// Lógica para Aparelho Auditivo
	if usaAparelhoAuditivo {
		instructions = strings.ReplaceAll(instructions, "{{#usa_aparelho_auditivo}}", "")
		instructions = strings.ReplaceAll(instructions, "{{/usa_aparelho_auditivo}}", "")
	} else {
		start := strings.Index(instructions, "{{#usa_aparelho_auditivo}}")
		end := strings.Index(instructions, "{{/usa_aparelho_auditivo}}")
		if start != -1 && end != -1 {
			instructions = instructions[:start] + instructions[end+len("{{/usa_aparelho_auditivo}}"):]
		}
	}

	// Limpeza de placeholders residuais e constantes de negócio
	instructions = strings.ReplaceAll(instructions, "{{#primeira_interacao}}", "")
	instructions = strings.ReplaceAll(instructions, "{{/primeira_interacao}}", "")
	instructions = strings.ReplaceAll(instructions, "{{^primeira_interacao}}", "")
	instructions = strings.ReplaceAll(instructions, "{{taxa_adesao}}", "85")

	return instructions, nil
}

func (h *PCMWebSocketHandler) listenGeminiResponses(client *PCMClient) {
	for {
		select {
		case <-client.ctx.Done():
			return
		default:
			if !client.active {
				return
			}

			response, err := client.GeminiClient.ReadResponse()
			if err != nil {
				if client.ctx.Err() != nil {
					return
				}
				// Pequeno backoff para não fritar a CPU em erro de leitura
				time.Sleep(100 * time.Millisecond)
				continue
			}

			h.handleGeminiResponse(client, response)
		}
	}
}

func (h *PCMWebSocketHandler) handleGeminiResponse(client *PCMClient, response map[string]interface{}) {
	// Ignorar mensagens puramente de controle de setup
	if setupComplete, ok := response["setupComplete"].(bool); ok && setupComplete {
		log.Printf("✅ Gemini Setup Concluído via WebSocket")
		return
	}

	serverContent, ok := response["serverContent"].(map[string]interface{})
	if !ok {
		return
	}

	modelTurn, ok := serverContent["modelTurn"].(map[string]interface{})
	if !ok {
		return
	}

	parts, ok := modelTurn["parts"].([]interface{})
	if !ok {
		return
	}

	for _, part := range parts {
		partMap, ok := part.(map[string]interface{})
		if !ok {
			continue
		}

		// 1. Processamento de Áudio PCM vindo da IA
		if inlineData, ok := partMap["inlineData"].(map[string]interface{}); ok {
			mimeType, _ := inlineData["mimeType"].(string)
			data, hasData := inlineData["data"].(string)

			if hasData && strings.HasPrefix(mimeType, "audio/pcm") {
				audioData, err := base64.StdEncoding.DecodeString(data)
				if err != nil {
					log.Printf("❌ Erro decode base64 áudio: %v", err)
					continue
				}

				// Envia o chunk de áudio para o canal de saída do cliente
				select {
				case client.SendCh <- audioData:
				case <-time.After(1 * time.Second):
					log.Printf("⚠️ Timeout enviando áudio para SendCh")
				}
			}
		}

		// 2. Processamento de Chamada de Ferramentas (Function Calling)
		if fnCall, ok := partMap["functionCall"].(map[string]interface{}); ok {
			h.executeTool(client, fnCall)
		}
	}
}

func (h *PCMWebSocketHandler) executeTool(client *PCMClient, fnCall map[string]interface{}) {
	name, _ := fnCall["name"].(string)
	args, _ := fnCall["args"].(map[string]interface{})

	log.Printf("🛠️ IA solicitou ferramenta: %s", name)

	switch name {
	case "alert_family":
		reason, _ := args["reason"].(string)
		log.Printf("🚨 Alerta de emergência! Razão: %s", reason)

		if err := gemini.AlertFamily(h.db.GetConnection(), h.pushService, client.IdosoID, reason); err != nil {
			log.Printf("❌ Erro ao disparar alerta para família: %v", err)
		}

	case "confirm_medication":
		medication, _ := args["medication_name"].(string)
		log.Printf("💊 Confirmação de remédio: %s", medication)

		if err := gemini.ConfirmMedication(h.db.GetConnection(), h.pushService, client.IdosoID, medication); err != nil {
			log.Printf("❌ Erro ao registrar medicamento no DB: %v", err)
		}
	}
}

func (h *PCMWebSocketHandler) sendJSON(client *PCMClient, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	if err := client.Conn.WriteMessage(websocket.TextMessage, jsonData); err != nil {
		log.Printf("❌ Erro ao enviar JSON: %v", err)
	}
}

func (h *PCMWebSocketHandler) sendError(client *PCMClient, message string) {
	h.sendJSON(client, map[string]interface{}{
		"type":    "error",
		"message": message,
	})
}

func (h *PCMWebSocketHandler) GetActiveClientsCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
