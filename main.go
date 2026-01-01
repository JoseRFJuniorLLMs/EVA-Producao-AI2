package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"eva-mind/internal/config"
	"eva-mind/internal/database"
	"eva-mind/internal/gemini"
	"eva-mind/internal/push"
	"eva-mind/internal/scheduler"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
)

// --- ESTRUTURAS CORE ---

type SignalingServer struct {
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

var (
	db              *database.DB
	pushService     *push.FirebaseService
	signalingServer *SignalingServer
	startTime       time.Time
	serverLogs      []string
	logsMutex       sync.RWMutex
)

const maxLogs = 100

type logWriter struct{}

func (lw logWriter) Write(p []byte) (n int, err error) {
	logsMutex.Lock()
	defer logsMutex.Unlock()

	msg := string(p)
	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}

	timestamp := time.Now().Format("15:04:05")
	logEntry := fmt.Sprintf("[%s] %s", timestamp, msg)

	serverLogs = append(serverLogs, logEntry)
	if len(serverLogs) > maxLogs {
		serverLogs = serverLogs[1:]
	}

	// Imprimir no console também
	fmt.Println(logEntry)

	return len(p), nil
}

// --- FUNÇÕES DE LOG ---

func addServerLog(msg string) {
	log.Println(msg)
}

// --- INICIALIZAÇÃO ---

func NewSignalingServer(cfg *config.Config, db *database.DB, pushService *push.FirebaseService) *SignalingServer {
	return &SignalingServer{
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  8192,
			WriteBufferSize: 8192,
		},
		clients:     make(map[string]*PCMClient),
		cfg:         cfg,
		pushService: pushService,
		db:          db,
	}
}

func main() {
	log.SetFlags(0)
	log.SetOutput(logWriter{})

	startTime = time.Now()
	addServerLog("🚀🚀🚀 SERVIDOR EVA-Mind COM LOGS MASSIVOS ULTRA VERBOSE v2.0 🚀🚀🚀")
	addServerLog("📊 MODO: LOGGING EXAUSTIVO ATIVADO - Todos os bytes serão logados!")
	addServerLog("🔍 Versão: ULTRA-VERBOSE-2026-01-01")
	addServerLog("⚡ ATENÇÃO: Esta versão loga TUDO - áudio binário, transcrições, hex dumps!")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("❌ Erro config: %v", err)
	}

	db, err = database.NewDB(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("❌ Erro DB: %v", err)
	}
	defer db.Close()

	pushService, err = push.NewFirebaseService(cfg.FirebaseCredentialsPath)
	if err != nil {
		addServerLog(fmt.Sprintf("⚠️ Aviso: Falha ao carregar Firebase: %v", err))
	} else {
		addServerLog("✅ Firebase inicializado com sucesso")
	}

	signalingServer = NewSignalingServer(cfg, db, pushService)

	sch, err := scheduler.NewScheduler(cfg, db.GetConnection())
	if err != nil {
		addServerLog(fmt.Sprintf("⚠️ Erro ao criar scheduler: %v", err))
	} else if sch != nil {
		go sch.Start(context.Background())
		addServerLog("✅ Scheduler iniciado")
	}

	router := mux.NewRouter()
	router.HandleFunc("/wss", signalingServer.HandleWebSocket)
	router.HandleFunc("/ws/pcm", signalingServer.HandleWebSocket) // Legado para App Android

	api := router.PathPrefix("/api").Subrouter()
	api.HandleFunc("/stats", statsHandler).Methods("GET")
	api.HandleFunc("/health", healthCheckHandler).Methods("GET")
	api.HandleFunc("/logs", logsHandler).Methods("GET")

	router.PathPrefix("/").Handler(http.FileServer(http.Dir("./web")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addServerLog(fmt.Sprintf("✅ Servidor pronto na porta %s", port))
	addServerLog("🎯 LOGS MASSIVOS ATIVADOS - Aguardando conexões para logar TUDO!")
	addServerLog("=" + "="*70)
	log.Fatal(http.ListenAndServe(":"+port, corsMiddleware(router)))
}

// --- WEBSOCKET ---

func (s *SignalingServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	addServerLog(fmt.Sprintf("🔌 Nova conexão WebSocket de %s", r.RemoteAddr))
	addServerLog(fmt.Sprintf("📍 Path: %s | User-Agent: %s", r.URL.Path, r.UserAgent()))

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		addServerLog(fmt.Sprintf("❌ Erro upgrade: %v", err))
		return
	}

	addServerLog("✅ WebSocket upgrade bem-sucedido")

	ctx, cancel := context.WithCancel(context.Background())
	client := &PCMClient{
		Conn:   conn,
		SendCh: make(chan []byte, 512),
		ctx:    ctx,
		cancel: cancel,
	}

	addServerLog("🚀 Iniciando goroutines de cliente...")
	go s.handleClientSend(client)
	s.handleClientMessages(client)
}

func (s *SignalingServer) handleClientMessages(client *PCMClient) {
	defer s.cleanupClient(client)
	addServerLog("📨 Iniciando loop de mensagens do cliente")

	for {
		msgType, message, err := client.Conn.ReadMessage()
		if err != nil {
			addServerLog(fmt.Sprintf("⚠️ Erro ao ler mensagem (CPF: %s): %v", client.CPF, err))
			break
		}

		if msgType == websocket.TextMessage {
			addServerLog(fmt.Sprintf("📩 Mensagem TEXT recebida: %s", string(message)))
			var data map[string]interface{}
			if err := json.Unmarshal(message, &data); err != nil {
				addServerLog(fmt.Sprintf("❌ Erro ao fazer unmarshal JSON: %v", err))
				continue
			}

			msgType, _ := data["type"].(string)
			addServerLog(fmt.Sprintf("🔍 Tipo de mensagem: %s", msgType))

			switch data["type"] {
			case "register":
				addServerLog("📝 Processando registro de cliente...")
				s.registerClient(client, data)
			case "start_call":
				addServerLog("📞 Solicitação de início de chamada")
				if client.CPF == "" {
					addServerLog("❌ Cliente não registrado tentou iniciar chamada")
					s.sendJSON(client, map[string]string{"type": "error", "message": "Registre-se primeiro"})
					continue
				}
				s.startGeminiSession(client)
			case "hangup":
				addServerLog(fmt.Sprintf("📴 Hangup recebido de %s", client.CPF))
				return
			default:
				addServerLog(fmt.Sprintf("⚠️ Tipo de mensagem desconhecido: %v", data["type"]))
			}
		}

		if msgType == websocket.BinaryMessage && client.active {
			addServerLog(fmt.Sprintf("🎤 Áudio BINARY recebido: %d bytes (CPF: %s)", len(message), client.CPF))
			if client.GeminiClient != nil {
				addServerLog("📤 Encaminhando áudio para Gemini...")
				client.GeminiClient.SendAudio(message)
			} else {
				addServerLog("⚠️ GeminiClient é nil, áudio descartado")
			}
		}
	}
	addServerLog(fmt.Sprintf("🔚 Loop de mensagens finalizado para %s", client.CPF))
}

func (s *SignalingServer) registerClient(client *PCMClient, data map[string]interface{}) {
	cpf, _ := data["cpf"].(string)
	addServerLog(fmt.Sprintf("🔍 Tentando registrar CPF: %s", cpf))
	addServerLog(fmt.Sprintf("📋 Dados recebidos: %+v", data))

	addServerLog("🗄️ Consultando banco de dados...")
	idoso, err := s.db.GetIdosoByCPF(cpf)
	if err != nil {
		addServerLog(fmt.Sprintf("❌ CPF não encontrado no banco: %s (erro: %v)", cpf, err))
		s.sendJSON(client, map[string]string{"type": "error", "message": "CPF não cadastrado"})
		return
	}

	addServerLog(fmt.Sprintf("✅ Idoso encontrado: ID=%d, Nome=%s, Ativo=%v", idoso.ID, idoso.Nome, idoso.Ativo))

	client.CPF = idoso.CPF
	client.IdosoID = idoso.ID

	s.mu.Lock()
	s.clients[idoso.CPF] = client
	addServerLog(fmt.Sprintf("📊 Total de clientes ativos: %d", len(s.clients)))
	s.mu.Unlock()

	addServerLog("📤 Enviando confirmação de registro...")
	s.sendJSON(client, map[string]string{"type": "registered"})
	addServerLog(fmt.Sprintf("✅ Cliente registrado: %s", cpf))
}

func (s *SignalingServer) startGeminiSession(client *PCMClient) {
	addServerLog(fmt.Sprintf("🤖 Iniciando sessão Gemini para %s (ID: %d)", client.CPF, client.IdosoID))

	addServerLog("🔌 Criando cliente Gemini...")
	gemClient, err := gemini.NewClient(client.ctx, s.cfg)
	if err != nil {
		addServerLog(fmt.Sprintf("❌ Erro ao criar cliente Gemini: %v", err))
		s.sendJSON(client, map[string]string{"type": "error", "message": "Erro IA"})
		return
	}
	addServerLog("✅ Cliente Gemini criado com sucesso")
	client.GeminiClient = gemClient

	addServerLog("📝 Construindo prompt personalizado...")
	instructions := s.buildPrompt(client.IdosoID)
	addServerLog(fmt.Sprintf("📋 Prompt: %s", instructions))

	addServerLog("🛠️ Carregando ferramentas (tools)...")
	tools := gemini.GetDefaultTools()
	addServerLog(fmt.Sprintf("🔧 Total de tools: %d", len(tools)))

	addServerLog("📤 Enviando setup para Gemini...")
	client.GeminiClient.SendSetup(instructions, tools)

	addServerLog("👂 Iniciando listener Gemini em goroutine...")
	go s.listenGemini(client)

	client.active = true
	addServerLog("✅ Cliente marcado como ATIVO")

	addServerLog("📤 Enviando confirmação session_created para cliente...")
	s.sendJSON(client, map[string]string{"type": "session_created", "status": "ready"})
	addServerLog(fmt.Sprintf("👤 Sessão Gemini COMPLETA: %s", client.CPF))
}

func (s *SignalingServer) buildPrompt(idosoID int64) string {
	var nome, tom string
	s.db.GetConnection().QueryRow("SELECT nome, tom_voz FROM idosos WHERE id = $1", idosoID).Scan(&nome, &tom)

	if tom == "" {
		tom = "calmo e acolhedor"
	}
	return fmt.Sprintf("Você é a EVA, assistente virtual para idosos. Ajude o(a) %s. Use tom %s.", nome, tom)
}

func (s *SignalingServer) listenGemini(client *PCMClient) {
	addServerLog(fmt.Sprintf("👂 Listener Gemini INICIADO para %s", client.CPF))
	for client.active {
		addServerLog(fmt.Sprintf("⏳ Aguardando resposta do Gemini (CPF: %s)...", client.CPF))
		resp, err := client.GeminiClient.ReadResponse()
		if err != nil {
			addServerLog(fmt.Sprintf("⚠️ Erro leitura Gemini (CPF: %s): %v", client.CPF, err))
			continue
		}
		addServerLog(fmt.Sprintf("📥 Resposta Gemini recebida para %s", client.CPF))
		s.processGeminiResponse(client, resp)
	}
	addServerLog(fmt.Sprintf("🔚 Listener Gemini FINALIZADO para %s", client.CPF))
}

func (s *SignalingServer) processGeminiResponse(client *PCMClient, resp map[string]interface{}) {
	addServerLog(fmt.Sprintf("🔄 Processando resposta Gemini para %s", client.CPF))

	serverContent, ok := resp["serverContent"].(map[string]interface{})
	if !ok {
		addServerLog("⚠️ Resposta sem serverContent, ignorando")
		return
	}

	addServerLog("📦 serverContent encontrado")
	modelTurn, _ := serverContent["modelTurn"].(map[string]interface{})
	parts, _ := modelTurn["parts"].([]interface{})
	addServerLog(fmt.Sprintf("📋 Processando %d parts", len(parts)))

	audioCount := 0
	for i, part := range parts {
		p, ok := part.(map[string]interface{})
		if !ok {
			continue
		}

		if data, hasData := p["inlineData"]; hasData {
			addServerLog(fmt.Sprintf("🎵 Part %d contém inlineData (áudio)", i))
			b64, _ := data.(map[string]interface{})["data"].(string)
			addServerLog(fmt.Sprintf("📊 Base64 length: %d chars", len(b64)))

			audio, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				addServerLog(fmt.Sprintf("❌ Erro ao decodificar base64: %v", err))
				continue
			}

			addServerLog(fmt.Sprintf("🎵 Áudio decodificado: %d bytes", len(audio)))
			addServerLog(fmt.Sprintf("📤 Enviando áudio para canal SendCh (CPF: %s)", client.CPF))
			client.SendCh <- audio
			audioCount++
			addServerLog(fmt.Sprintf("✅ Áudio #%d enviado para canal", audioCount))
		}
	}

	if audioCount == 0 {
		addServerLog("⚠️ Nenhum áudio encontrado na resposta Gemini")
	} else {
		addServerLog(fmt.Sprintf("✅ Total de %d áudios processados", audioCount))
	}
}

func (s *SignalingServer) handleClientSend(client *PCMClient) {
	addServerLog(fmt.Sprintf("📡 Handler de envio iniciado para %s", client.CPF))
	sentCount := 0
	for {
		select {
		case <-client.ctx.Done():
			addServerLog(fmt.Sprintf("🛑 Contexto cancelado, finalizando envio para %s (total enviado: %d)", client.CPF, sentCount))
			return
		case audio := <-client.SendCh:
			sentCount++
			addServerLog(fmt.Sprintf("📥 Áudio #%d recebido do canal (%d bytes) para %s", sentCount, len(audio), client.CPF))

			client.mu.Lock()
			addServerLog(fmt.Sprintf("📤 Enviando áudio #%d via WebSocket...", sentCount))
			err := client.Conn.WriteMessage(websocket.BinaryMessage, audio)
			client.mu.Unlock()

			if err != nil {
				addServerLog(fmt.Sprintf("❌ Erro ao enviar áudio #%d: %v", sentCount, err))
				return
			}
			addServerLog(fmt.Sprintf("✅ Áudio #%d enviado com sucesso para %s", sentCount, client.CPF))
		}
	}
}

func (s *SignalingServer) GetActiveClientsCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

func (s *SignalingServer) cleanupClient(client *PCMClient) {
	addServerLog(fmt.Sprintf("🧹 Iniciando cleanup do cliente: %s", client.CPF))

	addServerLog("🛑 Cancelando contexto...")
	client.cancel()

	s.mu.Lock()
	addServerLog(fmt.Sprintf("🗑️ Removendo cliente da lista (CPF: %s)", client.CPF))
	delete(s.clients, client.CPF)
	addServerLog(fmt.Sprintf("📊 Clientes restantes: %d", len(s.clients)))
	s.mu.Unlock()

	addServerLog("🔌 Fechando conexão WebSocket...")
	client.Conn.Close()

	if client.GeminiClient != nil {
		addServerLog("🤖 Fechando cliente Gemini...")
		client.GeminiClient.Close()
	}

	addServerLog(fmt.Sprintf("✅ Cliente desconectado e limpo: %s", client.CPF))
}

func (s *SignalingServer) sendJSON(c *PCMClient, v interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Conn.WriteJSON(v)
}

// --- API HANDLERS ---

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, Accept")

		// Responde preflight imediatamente
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	dbStatus := false
	if db != nil && db.GetConnection() != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := db.GetConnection().PingContext(ctx); err == nil {
			dbStatus = true
		}
	}

	firebaseStatus := (pushService != nil)

	response := map[string]interface{}{
		"active_clients": signalingServer.GetActiveClientsCount(),
		"uptime":         formatDuration(time.Since(startTime)),
		"db_status":      dbStatus,
		"firebase_ok":    firebaseStatus,
		"timestamp":      time.Now().Unix(),
	}

	json.NewEncoder(w).Encode(response)
}

func logsHandler(w http.ResponseWriter, r *http.Request) {
	logsMutex.RLock()
	defer logsMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs": serverLogs,
	})
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	status := "healthy"
	httpStatus := http.StatusOK

	if err := db.GetConnection().Ping(); err != nil {
		status = "unhealthy"
		httpStatus = http.StatusServiceUnavailable
	}

	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(map[string]string{
		"status": status,
		"time":   time.Now().Format(time.RFC3339),
	})
}

func formatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
