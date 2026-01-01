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
	// Configurações de buffering
	minChunkSize      = 1280 // 80ms @ 16kHz (múltiplo de 640)
	maxBufferSize     = 6400 // 400ms máximo
	minSendInterval   = 80   // ms - evita envios muito rápidos
	processingTimeout = 5000 // ms - timeout se Gemini não responder
)

func NewClient(ctx context.Context, cfg *config.Config) (*Client, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	url := fmt.Sprintf("wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent?key=%s", cfg.GoogleAPIKey)

	log.Printf("🔌 Conectando ao Gemini WebSocket...")
	conn, resp, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		log.Printf("❌ Erro ao conectar Gemini WebSocket: %v", err)
		return nil, err
	}

	log.Printf("✅ Gemini WebSocket conectado - Status: %s", resp.Status)

	client := &Client{
		conn:         conn,
		cfg:          cfg,
		audioBuffer:  make([]byte, 0, maxBufferSize),
		lastSendTime: time.Now(),
		audioChan:    make(chan []byte, 64), // Buffer de 64 chunks
		stopChan:     make(chan struct{}),
	}

	// Iniciar worker de processamento de áudio
	go client.audioWorker(ctx)

	return client, nil
}

// audioWorker processa áudio em background com buffering inteligente
func (c *Client) audioWorker(ctx context.Context) {
	ticker := time.NewTicker(50 * time.Millisecond) // Verificar buffer a cada 50ms
	defer ticker.Stop()

	log.Printf("🔧 Audio Worker iniciado")

	for {
		select {
		case <-ctx.Done():
			log.Printf("🛑 Audio Worker finalizado (context)")
			return
		case <-c.stopChan:
			log.Printf("🛑 Audio Worker finalizado (stop)")
			return

		case audioChunk := <-c.audioChan:
			c.bufferAudio(audioChunk)

		case <-ticker.C:
			c.flushBufferIfReady()
		}
	}
}

// bufferAudio adiciona áudio ao buffer interno
func (c *Client) bufferAudio(chunk []byte) {
	c.bufferMu.Lock()
	defer c.bufferMu.Unlock()

	// Adicionar ao buffer
	c.audioBuffer = append(c.audioBuffer, chunk...)

	// Se buffer atingiu tamanho ideal, enviar imediatamente
	if len(c.audioBuffer) >= minChunkSize {
		c.flushBuffer()
	}
}

// flushBufferIfReady envia buffer se condições forem atendidas
func (c *Client) flushBufferIfReady() {
	c.bufferMu.Lock()
	defer c.bufferMu.Unlock()

	// Condições para enviar:
	// 1. Há dados no buffer
	// 2. Tempo mínimo desde último envio passou
	// 3. Gemini não está processando (ou timeout)

	if len(c.audioBuffer) == 0 {
		return
	}

	timeSinceLastSend := time.Since(c.lastSendTime).Milliseconds()

	if timeSinceLastSend < minSendInterval {
		return // Muito rápido, aguardar
	}

	// Verificar se está processando há muito tempo (possível travamento)
	c.processingMu.Lock()
	processingTooLong := c.isProcessing && timeSinceLastSend > processingTimeout
	if processingTooLong {
		log.Printf("⚠️ Gemini não respondeu em %dms, forçando novo envio", processingTimeout)
		c.isProcessing = false
	}
	c.processingMu.Unlock()

	c.flushBuffer()
}

// flushBuffer envia o buffer atual (deve ser chamado com lock)
func (c *Client) flushBuffer() {
	if len(c.audioBuffer) == 0 {
		return
	}

	// Verificar se já está processando
	c.processingMu.Lock()
	if c.isProcessing {
		c.processingMu.Unlock()
		return // Aguardar resposta anterior
	}
	c.isProcessing = true
	c.processingMu.Unlock()

	// Copiar buffer para enviar
	toSend := make([]byte, len(c.audioBuffer))
	copy(toSend, c.audioBuffer)

	// Limpar buffer
	c.audioBuffer = c.audioBuffer[:0]
	c.lastSendTime = time.Now()

	// Enviar de forma assíncrona para não bloquear
	go c.sendAudioInternal(toSend)
}

func (c *Client) SendSetup(instructions string, tools []interface{}) error {
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
					"language_code": "pt-BR",
				},
			},
			"system_instruction": map[string]interface{}{
				"parts": []map[string]string{
					{"text": instructions},
				},
			},
			"tools": tools,
		},
	}

	log.Printf("📤 Enviando Setup para Gemini (Voice: Aoede, Lang: pt-BR)")

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.conn.WriteJSON(setupMsg); err != nil {
		log.Printf("❌ Erro ao enviar setup: %v", err)
		return fmt.Errorf("failed to send setup: %w", err)
	}

	log.Printf("✅ Setup enviado com sucesso")
	return nil
}

// SendAudio envia áudio para o canal de processamento (não-bloqueante)
func (c *Client) SendAudio(audioData []byte) error {
	if len(audioData) == 0 {
		return nil
	}

	// Enviar para canal (não bloqueia se buffer estiver cheio)
	select {
	case c.audioChan <- audioData:
		// OK
	default:
		log.Printf("⚠️ Canal de áudio cheio, descartando chunk de %d bytes", len(audioData))
	}

	return nil
}

// sendAudioInternal envia áudio diretamente para Gemini
func (c *Client) sendAudioInternal(audioData []byte) error {
	log.Printf("🎤 Enviando %d bytes para Gemini", len(audioData))

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
		log.Printf("❌ Erro ao enviar áudio: %v", err)

		// Liberar flag de processamento
		c.processingMu.Lock()
		c.isProcessing = false
		c.processingMu.Unlock()

		return err
	}

	log.Printf("✅ Áudio enviado com sucesso (%d bytes)", len(audioData))
	return nil
}

func (c *Client) ReadResponse() (map[string]interface{}, error) {
	var response map[string]interface{}
	err := c.conn.ReadJSON(&response)

	// Liberar flag de processamento quando receber resposta
	c.processingMu.Lock()
	c.isProcessing = false
	c.processingMu.Unlock()

	if err != nil {
		log.Printf("❌ Erro ao ler resposta: %v", err)
		return nil, err
	}

	log.Printf("📥 Resposta recebida do Gemini")

	// Log de transcriação se houver
	if serverContent, ok := response["serverContent"].(map[string]interface{}); ok {
		// Transcriação do usuário
		if userContent, ok := serverContent["userContent"].(map[string]interface{}); ok {
			if parts, ok := userContent["parts"].([]interface{}); ok {
				for _, part := range parts {
					if partMap, ok := part.(map[string]interface{}); ok {
						if text, ok := partMap["text"].(string); ok && text != "" {
							log.Printf("🎤 USUÁRIO: \"%s\"", text)
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
							log.Printf("🗣️ EVA: \"%s\"", text)
						}
					}
				}
			}
		}
	}

	return response, nil
}

func (c *Client) Close() error {
	log.Printf("🔌 Fechando Gemini Client...")

	close(c.stopChan)

	// Flush final do buffer
	c.bufferMu.Lock()
	if len(c.audioBuffer) > 0 {
		log.Printf("📤 Enviando %d bytes finais...", len(c.audioBuffer))
		c.flushBuffer()
	}
	c.bufferMu.Unlock()

	if c.conn != nil {
		err := c.conn.Close()
		if err != nil {
			log.Printf("⚠️ Erro ao fechar conexão: %v", err)
		} else {
			log.Printf("✅ Conexão Gemini fechada")
		}
		return err
	}
	return nil
}
