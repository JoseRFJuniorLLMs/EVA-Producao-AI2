package gemini

import (
	"context"
	"encoding/base64"
	"eva-mind/internal/config"
	"fmt"
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
	conn, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return nil, err
	}

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

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.conn.WriteJSON(setupMsg); err != nil {
		return fmt.Errorf("failed to send setup: %w", err)
	}

	return nil
}

func (c *Client) SendAudio(audioData []byte) error {
	encoded := base64.StdEncoding.EncodeToString(audioData)

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

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(msg)
}

func (c *Client) ReadResponse() (map[string]interface{}, error) {
	var response map[string]interface{}
	err := c.conn.ReadJSON(&response)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
