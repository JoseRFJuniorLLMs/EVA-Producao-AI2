package gemini

import (
	"context"
	"encoding/base64"
	"eva-mind/internal/config"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	conn         *websocket.Conn
	mu           sync.Mutex
	cfg          *config.Config
	audioBuffer  []byte
	bufferMu     sync.Mutex
	lastSendTime time.Time
	isProcessing bool
	processingMu sync.Mutex
	audioChan    chan []byte
	stopChan     chan struct{}
}

const (
	minChunkSize      = 1600  // 100ms @ 16kHz - OTIMIZADO para resposta mais rápida
	maxBufferSize     = 16000 // 1s máximo
	minSendInterval   = 100   // ms - REDUZIDO para menor latência
	processingTimeout = 5000  // ms - AUMENTADO para evitar falsos positivos
)

func NewClient(ctx context.Context, cfg *config.Config) (*Client, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	url := fmt.Sprintf("wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent?key=%s", cfg.GoogleAPIKey)

	log.Printf("🔌 Conectando ao Gemini WebSocket...")
	conn, resp, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		log.Printf("❌ Erro ao conectar: %v", err)
		return nil, err
	}

	log.Printf("✅ Conectado - Status: %s", resp.Status)

	client := &Client{
		conn:         conn,
		cfg:          cfg,
		audioBuffer:  make([]byte, 0, maxBufferSize),
		lastSendTime: time.Now(),
		audioChan:    make(chan []byte, 256), // AUMENTADO para evitar bloqueios
		stopChan:     make(chan struct{}),
	}

	go client.audioWorker(ctx)
	return client, nil
}

func (c *Client) audioWorker(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	log.Printf("🔧 Audio Worker iniciado")

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopChan:
			return
		case audioChunk := <-c.audioChan:
			c.bufferAudio(audioChunk)
		case <-ticker.C:
			c.flushBufferIfReady()
		}
	}
}

func (c *Client) bufferAudio(chunk []byte) {
	c.bufferMu.Lock()
	defer c.bufferMu.Unlock()

	c.audioBuffer = append(c.audioBuffer, chunk...)

	// Enviar quando buffer atingir tamanho ideal
	if len(c.audioBuffer) >= minChunkSize {
		c.flushBuffer()
	}
}

func (c *Client) flushBufferIfReady() {
	c.bufferMu.Lock()
	defer c.bufferMu.Unlock()

	if len(c.audioBuffer) == 0 {
		return
	}

	timeSinceLastSend := time.Since(c.lastSendTime).Milliseconds()

	if timeSinceLastSend < minSendInterval {
		return
	}

	c.processingMu.Lock()
	processingTooLong := c.isProcessing && timeSinceLastSend > processingTimeout
	if processingTooLong {
		log.Printf("⚠️ Gemini travado, forçando flush")
		c.isProcessing = false
	}
	c.processingMu.Unlock()

	c.flushBuffer()
}

func (c *Client) flushBuffer() {
	if len(c.audioBuffer) == 0 {
		return
	}

	c.processingMu.Lock()
	if c.isProcessing {
		c.processingMu.Unlock()
		return
	}
	c.isProcessing = true
	c.processingMu.Unlock()

	toSend := make([]byte, len(c.audioBuffer))
	copy(toSend, c.audioBuffer)

	c.audioBuffer = c.audioBuffer[:0]
	c.lastSendTime = time.Now()

	go c.sendAudioInternal(toSend)
}

func (c *Client) SendSetup(instructions string, tools []interface{}) error {
	// ============================================================
	// CORREÇÃO CRÍTICA: Forçar resposta em PORTUGUÊS
	// ============================================================
	setupMsg := map[string]interface{}{
		"setup": map[string]interface{}{
			"model": fmt.Sprintf("models/%s", c.cfg.ModelID),
			"generation_config": map[string]interface{}{
				"response_modalities": []string{"AUDIO"},
				"speech_config": map[string]interface{}{
					"voice_config": map[string]interface{}{
						"prebuilt_voice_config": map[string]string{
							"voice_name": "Aoede",
						},
					},
					// IMPORTANTE: Forçar português brasileiro
					"language_code": "pt-BR",
				},
			},
			"system_instruction": map[string]interface{}{
				"parts": []map[string]string{
					{
						// CORREÇÃO: Instruções mais explícitas
						"text": fmt.Sprintf(`%s

REGRAS OBRIGATÓRIAS:
1. Responda SEMPRE em português brasileiro
2. NUNCA responda em inglês
3. Use tom de voz natural e acolhedor
4. Seja breve e direta
5. Fale como uma pessoa real, não como IA
6. NUNCA inclua markdown ou formatação
7. NUNCA diga "Embracing" ou palavras em inglês`, instructions),
					},
				},
			},
			"tools": tools,
		},
	}

	log.Printf("📤 Enviando Setup para Gemini...")
	log.Printf("🗣️ Voice: Aoede | Language: pt-BR")

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.conn.WriteJSON(setupMsg); err != nil {
		log.Printf("❌ Erro ao enviar setup: %v", err)
		return fmt.Errorf("failed to send setup: %w", err)
	}

	log.Printf("✅ Setup enviado")
	return nil
}

func (c *Client) SendAudio(audioData []byte) error {
	if len(audioData) == 0 {
		return nil
	}

	select {
	case c.audioChan <- audioData:
		// OK
	default:
		log.Printf("⚠️ Canal cheio, descartando chunk")
	}

	return nil
}

func (c *Client) sendAudioInternal(audioData []byte) error {
	// Log reduzido para performance
	if len(audioData) > 10000 {
		log.Printf("🎤 Enviando %d bytes para Gemini", len(audioData))
	}

	encoded := base64.StdEncoding.EncodeToString(audioData)

	msg := map[string]interface{}{
		"realtime_input": map[string]interface{}{
			"media_chunks": []map[string]string{
				{
					"mime_type": "audio/pcm;rate=16000",
					"data":      encoded,
				},
			},
		},
	}

	c.mu.Lock()
	err := c.conn.WriteJSON(msg)
	c.mu.Unlock()

	if err != nil {
		log.Printf("❌ Erro ao enviar: %v", err)

		c.processingMu.Lock()
		c.isProcessing = false
		c.processingMu.Unlock()

		return err
	}

	// Log reduzido para performance
	if len(audioData) > 10000 {
		log.Printf("✅ Áudio enviado com sucesso (%d bytes)", len(audioData))
	}
	return nil
}

func (c *Client) ReadResponse() (map[string]interface{}, error) {
	var response map[string]interface{}
	err := c.conn.ReadJSON(&response)

	c.processingMu.Lock()
	c.isProcessing = false
	c.processingMu.Unlock()

	if err != nil {
		log.Printf("❌ Erro ao ler resposta: %v", err)
		return nil, err
	}

	log.Printf("📥 Resposta recebida do Gemini")

	// Log de transcrições
	if serverContent, ok := response["serverContent"].(map[string]interface{}); ok {
		// Transcrição do usuário
		if userContent, ok := serverContent["userContent"].(map[string]interface{}); ok {
			if parts, ok := userContent["parts"].([]interface{}); ok {
				for _, part := range parts {
					if partMap, ok := part.(map[string]interface{}); ok {
						if text, ok := partMap["text"].(string); ok && text != "" {
							log.Printf("🎤 USUÁRIO: \"%s\"", text)

							// ============================================================
							// VERIFICAÇÃO: Se EVA responder em inglês, alertar!
							// ============================================================
							if containsEnglishMarkers(text) {
								log.Printf("⚠️⚠️⚠️ AVISO: Resposta contém inglês! Verificar prompt!")
							}
						}
					}
				}
			}
		}

		// Resposta da EVA
		if modelTurn, ok := serverContent["modelTurn"].(map[string]interface{}); ok {
			if parts, ok := modelTurn["parts"].([]interface{}); ok {
				for _, part := range parts {
					if partMap, ok := part.(map[string]interface{}); ok {
						if text, ok := partMap["text"].(string); ok && text != "" {
							// ============================================================
							// FILTRAR: Não logar se for só markdown/formatação
							// ============================================================
							if !isMarkdownOnly(text) {
								log.Printf("🗣️ EVA: \"%s\"", text)
							}

							if containsEnglishMarkers(text) {
								log.Printf("🚨🚨🚨 CRÍTICO: EVA respondeu em INGLÊS!")
							}
						}
					}
				}
			}
		}
	}

	return response, nil
}

// containsEnglishMarkers detecta se texto contém palavras em inglês comuns
func containsEnglishMarkers(text string) bool {
	englishWords := []string{
		"Embracing", "User", "Interaction", "I've", "I'm",
		"Offering", "welcome", "registered", "greeting",
	}

	for _, word := range englishWords {
		if contains(text, word) {
			return true
		}
	}
	return false
}

// isMarkdownOnly verifica se é só formatação markdown
func isMarkdownOnly(text string) bool {
	return contains(text, "**") && len(text) < 100
}

func contains(text, substr string) bool {
	return len(text) >= len(substr) &&
		(text[:len(substr)] == substr ||
			contains(text[1:], substr))
}

func (c *Client) Close() error {
	log.Printf("🔌 Fechando Gemini Client...")

	close(c.stopChan)

	c.bufferMu.Lock()
	if len(c.audioBuffer) > 0 {
		log.Printf("📤 Enviando %d bytes finais...", len(c.audioBuffer))
		c.flushBuffer()
	}
	c.bufferMu.Unlock()

	if c.conn != nil {
		err := c.conn.Close()
		if err != nil {
			log.Printf("⚠️ Erro ao fechar: %v", err)
		} else {
			log.Printf("✅ Conexão fechada")
		}
		return err
	}
	return nil
}
