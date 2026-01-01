package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"eva-mind/internal/config"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	conn *websocket.Conn
	mu   sync.Mutex
	cfg  *config.Config
}

func NewClient(ctx context.Context, cfg *config.Config) (*Client, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	url := fmt.Sprintf("wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent?key=%s", cfg.GoogleAPIKey)

	log.Printf("🔌 Conectando ao Gemini WebSocket...")
	log.Printf("📍 URL: wss://generativelanguage.googleapis.com/ws/...")
	log.Printf("🤖 Model: %s", cfg.ModelID)

	conn, resp, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		log.Printf("❌ Erro ao conectar Gemini WebSocket: %v", err)
		return nil, err
	}

	log.Printf("✅ Gemini WebSocket conectado com sucesso")
	log.Printf("📊 Response Status: %s", resp.Status)

	return &Client{conn: conn, cfg: cfg}, nil
}

func (c *Client) SendSetup(instructions string, tools []interface{}) error {
	setupMsg := map[string]interface{}{
		"setup": map[string]interface{}{
			"model": fmt.Sprintf("models/%s", c.cfg.ModelID),
			"generation_config": map[string]interface{}{
				// Resposta em áudio (não texto)
				"response_modalities": []string{"AUDIO"},
				"speech_config": map[string]interface{}{
					"voice_config": map[string]interface{}{
						"prebuilt_voice_config": map[string]string{
							// Voz feminina brasileira
							"voice_name": "Aoede",
						},
					},
					// Detecção automática de início/fim de fala
					"voice_activity_detection_config": map[string]interface{}{
						"start_threshold": 0.5, // Sensibilidade de início (0.0-1.0)
						"end_threshold":   0.5, // Sensibilidade de fim (0.0-1.0)
						"enabled":         true,
					},
				},
				// IA proativa, inicia conversas
				"proactivity": map[string]bool{
					"proactive_audio": true,
				},
				// Habilita transcrição de texto do áudio
				"output_audio_transcription": map[string]interface{}{},
				// Ativa diálogo afetivo/emocional
				"enable_affective_dialog": true,
			},
			"system_instruction": map[string]interface{}{
				"parts": []map[string]string{
					{"text": instructions},
				},
			},
			"tools": tools,
		},
	}

	log.Printf("📤 Enviando Setup para Gemini...")
	log.Printf("🎙️ Response Modalities: AUDIO")
	log.Printf("🗣️ Voice: Aoede")
	log.Printf("🎯 Proactive Audio: ENABLED")
	log.Printf("📝 Instructions length: %d chars", len(instructions))

	// Log do JSON completo (para debug)
	setupJSON, _ := json.MarshalIndent(setupMsg, "", "  ")
	log.Printf("📋 Setup JSON:\n%s", string(setupJSON))

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.conn.WriteJSON(setupMsg); err != nil {
		log.Printf("❌ Erro ao enviar setup: %v", err)
		return fmt.Errorf("failed to send setup: %w", err)
	}

	log.Printf("✅ Setup enviado com sucesso para Gemini")
	return nil
}

func (c *Client) SendAudio(audioData []byte) error {
	log.Printf("🎤 ========================================")
	log.Printf("🎤 ENVIANDO ÁUDIO PARA GEMINI")
	log.Printf("🎤 ========================================")
	log.Printf("📊 Tamanho do áudio RAW: %d bytes", len(audioData))

	// Mostrar primeiros bytes do áudio (hexadecimal)
	if len(audioData) > 0 {
		previewLen := 32
		if len(audioData) < previewLen {
			previewLen = len(audioData)
		}
		log.Printf("🔍 Primeiros %d bytes (HEX): % X", previewLen, audioData[:previewLen])
		log.Printf("🔍 Primeiros %d bytes (DEC): %v", previewLen, audioData[:previewLen])
	}

	log.Printf("🔄 Codificando para Base64...")
	encoded := base64.StdEncoding.EncodeToString(audioData)
	log.Printf("📊 Tamanho Base64: %d chars", len(encoded))
	log.Printf("🔍 Base64 preview (primeiros 100 chars): %s...", encoded[:min(100, len(encoded))])

	msg := map[string]interface{}{
		"realtime_input": map[string]interface{}{
			"media_chunks": []map[string]string{
				{
					"mime_type": "audio/pcm",
					"data":      encoded,
				},
			},
			// Habilitar transcrição de entrada (áudio do usuário)
			"input_audio_transcription": map[string]interface{}{
				"enabled": true,
			},
		},
	}

	log.Printf("📤 Enviando JSON para Gemini WebSocket...")
	log.Printf("📋 Payload structure: realtime_input.media_chunks[0].mime_type = audio/pcm")
	log.Printf("📋 Payload structure: realtime_input.input_audio_transcription.enabled = true")

	c.mu.Lock()
	defer c.mu.Unlock()

	err := c.conn.WriteJSON(msg)
	if err != nil {
		log.Printf("❌ ERRO ao enviar áudio para Gemini: %v", err)
		return err
	}

	log.Printf("✅ Áudio enviado com SUCESSO para Gemini!")
	log.Printf("🎤 ========================================")
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (c *Client) ReadResponse() (map[string]interface{}, error) {
	var response map[string]interface{}
	err := c.conn.ReadJSON(&response)
	if err != nil {
		log.Printf("❌ Erro ao ler resposta do Gemini: %v", err)
		return nil, err
	}

	// Log detalhado da resposta
	log.Printf("📥 ========================================")
	log.Printf("📥 RESPOSTA RECEBIDA DO GEMINI")

	// Verificar tipo de resposta
	if setupComplete, ok := response["setupComplete"].(bool); ok && setupComplete {
		log.Printf("✅ Setup Complete confirmado pelo Gemini")
	}

	if serverContent, ok := response["serverContent"].(map[string]interface{}); ok {
		log.Printf("📦 serverContent detectado")

		// ============================================================
		// NOVO: CAPTURAR TRANSCRIÇÃO DO USUÁRIO (Input Audio)
		// ============================================================
		if turnComplete, ok := serverContent["turnComplete"].(bool); ok && turnComplete {
			log.Printf("🔄 Turn Complete detectado")
		}

		// Verificar se há transcrição do áudio de ENTRADA (usuário falando)
		if interrupted, ok := serverContent["interrupted"].(bool); ok {
			log.Printf("⚠️ Interrupted: %v", interrupted)
		}

		// Capturar transcrição do usuário
		if grounding, ok := serverContent["groundingMetadata"].(map[string]interface{}); ok {
			log.Printf("🔍 Grounding Metadata detectado: %v", grounding)
		}

		if modelTurn, ok := serverContent["modelTurn"].(map[string]interface{}); ok {
			log.Printf("🤖 modelTurn detectado")

			if parts, ok := modelTurn["parts"].([]interface{}); ok {
				log.Printf("📋 Parts count: %d", len(parts))

				for i, part := range parts {
					partMap, _ := part.(map[string]interface{})

					// ============================================================
					// CAPTURAR TEXTO/TRANSCRIÇÃO DA EVA
					// ============================================================
					if text, ok := partMap["text"].(string); ok {
						log.Printf("🗣️ ========================================")
						log.Printf("🗣️ EVA DISSE (Part %d):", i)
						log.Printf("🗣️ \"%s\"", text)
						log.Printf("🗣️ ========================================")
					}

					// Verificar se tem áudio
					if inlineData, ok := partMap["inlineData"].(map[string]interface{}); ok {
						mimeType, _ := inlineData["mimeType"].(string)
						data, hasData := inlineData["data"].(string)

						log.Printf("🎵 Part %d: inlineData encontrado", i)
						log.Printf("   - mimeType: %s", mimeType)
						log.Printf("   - hasData: %v", hasData)

						if hasData {
							log.Printf("   - data length (base64): %d chars", len(data))
						}
					}

					// Verificar se tem function call
					if fnCall, ok := partMap["functionCall"].(map[string]interface{}); ok {
						fnName, _ := fnCall["name"].(string)
						log.Printf("�️ Part %d: functionCall: %s", i, fnName)
					}
				}
			}
		}

		// ============================================================
		// NOVO: CAPTURAR TRANSCRIÇÃO DO ÁUDIO DO USUÁRIO
		// ============================================================
		if userContent, ok := serverContent["userContent"].(map[string]interface{}); ok {
			log.Printf("👤 userContent detectado")

			if parts, ok := userContent["parts"].([]interface{}); ok {
				log.Printf("👤 User Parts count: %d", len(parts))

				for i, part := range parts {
					partMap, _ := part.(map[string]interface{})

					// TRANSCRIÇÃO DO QUE O USUÁRIO FALOU
					if text, ok := partMap["text"].(string); ok {
						log.Printf("🎤 ========================================")
						log.Printf("🎤 USUÁRIO DISSE (Part %d):", i)
						log.Printf("🎤 \"%s\"", text)
						log.Printf("🎤 ========================================")
					}

					// Verificar se tem inlineData (áudio do usuário)
					if inlineData, ok := partMap["inlineData"].(map[string]interface{}); ok {
						mimeType, _ := inlineData["mimeType"].(string)
						log.Printf("🎤 User audio detected - mimeType: %s", mimeType)
					}
				}
			}
		}
	}

	// Log do JSON completo para debug extremo
	responseJSON, _ := json.MarshalIndent(response, "", "  ")
	log.Printf("📋 Response JSON completo:\n%s", string(responseJSON))
	log.Printf("📥 ========================================")

	return response, nil
}

func (c *Client) Close() error {
	log.Printf("🔌 Fechando conexão Gemini WebSocket...")
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
