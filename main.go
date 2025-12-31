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
	addServerLog("🚀 Iniciando Servidor EVA-Mind Completo...")

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
	log.Fatal(http.ListenAndServe(":"+port, corsMiddleware(router)))
}

// --- WEBSOCKET ---

func (s *SignalingServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		addServerLog(fmt.Sprintf("❌ Erro upgrade: %v", err))
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	client := &PCMClient{
		Conn:   conn,
		SendCh: make(chan []byte, 512),
		ctx:    ctx,
		cancel: cancel,
	}

	go s.handleClientSend(client)
	s.handleClientMessages(client)
}

func (s *SignalingServer) handleClientMessages(client *PCMClient) {
	defer s.cleanupClient(client)

	for {
		msgType, message, err := client.Conn.ReadMessage()
		if err != nil {
			break
		}

		if msgType == websocket.TextMessage {
			var data map[string]interface{}
			json.Unmarshal(message, &data)

			switch data["type"] {
			case "register":
				s.registerClient(client, data)
			case "start_call":
				if client.CPF == "" {
					s.sendJSON(client, map[string]string{"type": "error", "message": "Registre-se primeiro"})
					continue
				}
				s.startGeminiSession(client)
			case "hangup":
				return
			}
		}

		if msgType == websocket.BinaryMessage && client.active {
			if client.GeminiClient != nil {
				client.GeminiClient.SendAudio(message)
			}
		}
	}
}

func (s *SignalingServer) registerClient(client *PCMClient, data map[string]interface{}) {
	cpf, _ := data["cpf"].(string)

	idoso, err := s.db.GetIdosoByCPF(cpf)
	if err != nil {
		addServerLog(fmt.Sprintf("❌ CPF não encontrado: %s", cpf))
		s.sendJSON(client, map[string]string{"type": "error", "message": "CPF não cadastrado"})
		return
	}

	client.CPF = idoso.CPF
	client.IdosoID = idoso.ID

	s.mu.Lock()
	s.clients[idoso.CPF] = client
	s.mu.Unlock()

	s.sendJSON(client, map[string]string{"type": "registered"})
	addServerLog(fmt.Sprintf("✅ Cliente registrado: %s", cpf))
}

func (s *SignalingServer) startGeminiSession(client *PCMClient) {
	gemClient, err := gemini.NewClient(client.ctx, s.cfg)
	if err != nil {
		addServerLog(fmt.Sprintf("❌ Erro Gemini: %v", err))
		s.sendJSON(client, map[string]string{"type": "error", "message": "Erro IA"})
		return
	}
	client.GeminiClient = gemClient

	instructions := s.buildPrompt(client.IdosoID)
	tools := gemini.GetDefaultTools()

	client.GeminiClient.SendSetup(instructions, tools)
	go s.listenGemini(client)

	client.active = true

	s.sendJSON(client, map[string]string{"type": "session_created", "status": "ready"})
	addServerLog(fmt.Sprintf("👤 Sessão iniciada: %s", client.CPF))
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
	for client.active {
		resp, err := client.GeminiClient.ReadResponse()
		if err != nil {
			addServerLog(fmt.Sprintf("⚠️ Erro leitura Gemini: %v", err))
			continue
		}
		s.processGeminiResponse(client, resp)
	}
}

func (s *SignalingServer) processGeminiResponse(client *PCMClient, resp map[string]interface{}) {
	serverContent, ok := resp["serverContent"].(map[string]interface{})
	if !ok {
		return
	}

	modelTurn, _ := serverContent["modelTurn"].(map[string]interface{})
	parts, _ := modelTurn["parts"].([]interface{})

	for _, part := range parts {
		p, ok := part.(map[string]interface{})
		if !ok {
			continue
		}

		if data, hasData := p["inlineData"]; hasData {
			b64, _ := data.(map[string]interface{})["data"].(string)
			audio, _ := base64.StdEncoding.DecodeString(b64)
			client.SendCh <- audio
		}
	}
}

func (s *SignalingServer) handleClientSend(client *PCMClient) {
	for {
		select {
		case <-client.ctx.Done():
			return
		case audio := <-client.SendCh:
			client.mu.Lock()
			err := client.Conn.WriteMessage(websocket.BinaryMessage, audio)
			client.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (s *SignalingServer) GetActiveClientsCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

func (s *SignalingServer) cleanupClient(client *PCMClient) {
	client.cancel()
	s.mu.Lock()
	delete(s.clients, client.CPF)
	s.mu.Unlock()
	client.Conn.Close()
	addServerLog(fmt.Sprintf("🔌 Cliente desconectado: %s", client.CPF))
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
