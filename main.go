package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed index.html style.css
var embedded embed.FS

var jwtSecretKey []byte

var infoLog *log.Logger
var errorLog *log.Logger

var CONF Config
var SQLM Sqlm

type Sqlm struct {
	execConn *pgx.Conn
}

type Config struct {
	port            string
	password        string
	openRouterKey   string
	openRouterModel string
	execDB          string
}

type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openRouterRequest struct {
	Model          string          `json:"model"`
	Messages       []LLMMessage    `json:"messages"`
	ResponseFormat json.RawMessage `json:"response_format"`
}

const openRouterResponseFormat = `{
  "type": "json_schema",
  "json_schema": {
    "name": "SQLResponse",
    "strict": true,
    "schema": {
      "type": "object",
      "properties": {
        "outline": {
          "type": "string",
          "description": "A brief outline of the SQL query logic."
        },
        "sql": {
          "type": "string",
          "description": "The SQL query to execute."
        }
      },
      "required": ["outline", "sql"],
      "additionalProperties": false
    }
  }
}`

type openRouterResponse struct {
	Choices []struct {
		Message LLMMessage `json:"message"`
	} `json:"choices"`
}

func main() {
	infoLog = log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	errorLog = log.New(os.Stdout, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
	initConfig()
	jwtSecretKey = generateRandomKey(32)
	SQLM.initExecConn()
	defer SQLM.execConn.Close(context.Background())
	httpServer()
}

func initConfig() {
	CONF.port = ":8080"
	CONF.password = ""
	if port := os.Getenv("SQLM_PORT"); port != "" {
		CONF.port = ":" + port
	}
	CONF.password = os.Getenv("SQLM_PASSWORD")
	CONF.openRouterKey = strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	CONF.openRouterModel = strings.TrimSpace(os.Getenv("OPENROUTER_MODEL"))
	if CONF.openRouterKey == "" || CONF.openRouterModel == "" {
		log.Fatal("OPENROUTER_API_KEY and OPENROUTER_MODEL are required")
	}
	CONF.execDB = os.Getenv("SQLM_EXEC_DB")
	if CONF.execDB == "" {
		errorLog.Printf("SQLM_EXEC_DB is not set. SQL execution is not available.")
	}
}

func generateRandomKey(size int) []byte {
	key := make([]byte, size)
	_, err := rand.Read(key)
	if err != nil {
		errorLog.Printf("Failed to generate a JWT secret key. Aborting.")
		os.Exit(1)
	}
	return key
}

func (S *Sqlm) initExecConn() {
	if strings.TrimSpace(CONF.execDB) == "" {
		infoLog.Printf("No execution database configured.")
		return
	}
	//todo: set timeout from config
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, CONF.execDB)
	if err != nil {
		log.Fatalf("cannot open execution db connection: %v", err)
	}
	if err := conn.Ping(ctx); err != nil {
		log.Fatalf("cannot ping execution db connection: %v", err)
	}
	S.execConn = conn
}

// func (S *Sqlm) CreateMessage(chatID int, msgType int, text string, outline string, sql string) error {
// 	tx, err := S.db.Begin()
// 	if err != nil {
// 		return err
// 	}
// 	defer func() {
// 		if err != nil {
// 			tx.Rollback()
// 		}
// 	}()
// 	var chatExists int
// 	err = tx.QueryRow(`
// 		SELECT EXISTS(SELECT 1 FROM chats WHERE id = ?)
// 	`, chatID).Scan(&chatExists)
// 	if chatExists == 0 {
// 		errorLog.Printf("Chat with id=%d not found", chatID)
// 		return errors.New("chat not found")
// 	}
// 	var nextPos int
// 	err = tx.QueryRow(`
// 		SELECT COALESCE(MAX(position), -1) + 1
// 		FROM messages
// 		WHERE chat_id = ?
// 	`, chatID).Scan(&nextPos)
// 	if err != nil {
// 		return err
// 	}
// 	_, err = tx.Exec(`
// 		INSERT INTO messages (chat_id, position, type, text, outline, sql)
// 		VALUES (?, ?, ?, ?, ?, ?)
// 	`, chatID, nextPos, msgType, text, outline, sql)
// 	if err != nil {
// 		return err
// 	}
// 	_, err = tx.Exec(`
// 		UPDATE chats
// 		SET updated = CURRENT_TIMESTAMP
// 		WHERE id = ?
// 	`, chatID)
// 	if err != nil {
// 		return err
// 	}
// 	err = tx.Commit()
// 	return err
// }

func buildLLMMessages(msg string) []LLMMessage {
	out := []LLMMessage{
		{
			Role: "system",
			Content: `You are a SQL assistant. Answer briefly in the specified format.
				Return ONLY a valid JSON object with exactly two keys: 'outline' and 'sql'.
				'outline' must be a brief description of the query logic.
				'sql' must be the executable SQL query.
				"Do not include markdown, code fences, explanations, or any extra keys.
				"If requirements are ambiguous, still return valid JSON and put clarification needs in 'outline'.`,
		},
	}
	out = append(out, LLMMessage{
		Role:    "user",
		Content: msg,
	})
	return out
}

func callOpenRouter(messages []LLMMessage) (string, error) {
	reqBody := openRouterRequest{
		Model:          CONF.openRouterModel,
		Messages:       messages,
		ResponseFormat: json.RawMessage([]byte(openRouterResponseFormat)),
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+CONF.openRouterKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("openrouter status: %d", resp.StatusCode)
	}
	var orResp openRouterResponse
	err = json.NewDecoder(resp.Body).Decode(&orResp)
	if err != nil {
		errorLog.Printf("Failed to decode OpenRouter response: %v", err)
		return "", err
	}
	if len(orResp.Choices) == 0 || strings.TrimSpace(orResp.Choices[0].Message.Content) == "" {
		return "", errors.New("empty assistant response")
	}
	return strings.TrimSpace(orResp.Choices[0].Message.Content), nil
}

// func (S *Sqlm) ExecuteSQL(query string) ([]map[string]any, error) {
// 	query = strings.TrimSpace(query)
// 	if query == "" {
// 		return nil, errors.New("empty query")
// 	}
// 	if S.execConn == nil {
// 		return nil, errors.New("execution db not initialized")
// 	}
// 	//todo: set timeout from config
// 	//todo: pass user context for cancellation
// 	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
// 	defer cancel()
// 	tx, err := S.execConn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
// 	if err != nil {
// 		return nil, err
// 	}
// 	defer tx.Rollback(ctx)
// 	rows, err := tx.Query(ctx, query)
// 	if err != nil {
// 		return nil, err
// 	}
// 	defer rows.Close()

// 	fds := rows.FieldDescriptions()
// 	//todo: deal with large results.
// 	const maxRows = 50
// 	results := make([]map[string]any, 0, 50)
// 	for rows.Next() {
// 		if len(results) >= maxRows {
// 			break
// 		}
// 		values, err := rows.Values()
// 		if err != nil {
// 			return nil, err
// 		}
// 		row := make(map[string]any, len(values))
// 		for i, fd := range fds {
// 			v := values[i]
// 			if b, ok := v.([]byte); ok {
// 				row[string(fd.Name)] = string(b)
// 			} else {
// 				row[string(fd.Name)] = v
// 			}
// 		}
// 		results = append(results, row)
// 	}

// 	if err := rows.Err(); err != nil {
// 		return nil, err
// 	}
// 	if err := tx.Commit(ctx); err != nil {
// 		return nil, err
// 	}
// 	return results, nil
// }

func httpServer() {
	http.HandleFunc("/", httpIndex)
	http.Handle("/style.css", http.FileServer(http.FS(embedded)))
	http.HandleFunc("/login", httpLogin)
	http.HandleFunc("/checkauth", httpCheckAuthHandler)
	http.HandleFunc("/message", httpUserMessage)
	//http.HandleFunc("/execute", httpExecute)
	log.Fatal(http.ListenAndServe(CONF.port, nil))
}

func httpIndex(w http.ResponseWriter, r *http.Request) {
	data, err := embedded.ReadFile("index.html")
	if err != nil {
		http.Error(w, "Error loading chats", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write(data)
}

func httpLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}
	var creds struct {
		Password string `json:"password"`
	}
	err := json.NewDecoder(r.Body).Decode(&creds)
	if err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}
	if creds.Password != CONF.password {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}
	expirationTime := time.Now().Add(15 * time.Minute)
	claims := jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(expirationTime),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(jwtSecretKey)
	if err != nil {
		http.Error(w, "Failed to create token", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "token",
		Value:    tokenString,
		Expires:  expirationTime,
		HttpOnly: true,
	})
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Login successful!"))
}

func httpCheckAuth(w http.ResponseWriter, r *http.Request) (error, int, string) {
	if CONF.password == "" {
		return nil, http.StatusOK, "Ok"
	}
	cookie, err := r.Cookie("token")
	if err != nil {
		if err == http.ErrNoCookie {
			return err, http.StatusUnauthorized, "Unauthorized"
		}
		return err, http.StatusBadRequest, "Bad request"
	}
	tokenStr := cookie.Value
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		return jwtSecretKey, nil
	})
	if err != nil || !token.Valid {
		return err, http.StatusUnauthorized, "Unauthorized"
	}
	//todo: prolong token
	return nil, http.StatusOK, "Ok"
}

func httpCheckAuthHandler(w http.ResponseWriter, r *http.Request) {
	err, code, msg := httpCheckAuth(w, r)
	if err != nil {
		http.Error(w, msg, code)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Ok"))
}

func httpUserMessage(w http.ResponseWriter, r *http.Request) {
	err, code, msg := httpCheckAuth(w, r)
	if err != nil {
		http.Error(w, msg, code)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	// err = SQLM.CreateMessage(req.ChatID, 0, req.Text, "", "")
	// if err != nil {
	// 	errorLog.Printf("Failed to save user message: %v", err)
	// 	http.Error(w, "Failed to save message", http.StatusInternalServerError)
	// 	return
	// }

	assistantText, err := callOpenRouter(buildLLMMessages(req.Text))
	if err != nil {
		errorLog.Printf("OpenRouter request failed: %v", err)
		http.Error(w, "Assistant unavailable", http.StatusBadGateway)
		return
	}
	var parsed struct {
		Outline string `json:"outline"`
		SQL     string `json:"sql"`
	}
	err = json.Unmarshal([]byte(assistantText), &parsed)
	if err != nil {
		errorLog.Printf("Failed to parse assistant response: %v", err)
		http.Error(w, "Assistant returned invalid JSON", http.StatusBadGateway)
		return
	}
	outline := strings.TrimSpace(parsed.Outline)
	sql := strings.TrimSpace(parsed.SQL)
	//todo: format and validate SQL
	// err = SQLM.CreateMessage(req.ChatID, 1, assistantText, outline, sql)
	// if err != nil {
	// 	errorLog.Printf("Failed to save assistant message: %v", err)
	// 	http.Error(w, "Failed to save assistant message", http.StatusInternalServerError)
	// 	return
	// }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"outline": outline,
		"sql":     sql,
	})
}

// func httpExecute(w http.ResponseWriter, r *http.Request) {
// 	err, code, msg := httpCheckAuth(w, r)
// 	if err != nil {
// 		http.Error(w, msg, code)
// 		return
// 	}
// 	if r.Method != http.MethodPost {
// 		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
// 		return
// 	}
// 	var req struct {
// 		ChatID    int `json:"chat_id"`
// 		MessageID int `json:"message_id"`
// 	}
// 	err = json.NewDecoder(r.Body).Decode(&req)
// 	if err != nil {
// 		http.Error(w, "Invalid JSON", http.StatusBadRequest)
// 		return
// 	}
// 	if req.ChatID <= 0 || req.MessageID <= 0 {
// 		http.Error(w, "Missing or invalid chat_id/message_id", http.StatusBadRequest)
// 		return
// 	}
// 	m, err := SQLM.loadMessageByID(req.ChatID, req.MessageID)
// 	if err != nil {
// 		http.Error(w, "Message not found", http.StatusNotFound)
// 		return
// 	}
// 	if m.Type != 1 {
// 		http.Error(w, "Only assistant messages can be executed", http.StatusBadRequest)
// 		return
// 	}
// 	query := strings.TrimSpace(m.SQL)
// 	if query == "" {
// 		http.Error(w, "No SQL found on this message", http.StatusBadRequest)
// 		return
// 	}
// 	//todo: create context
// 	rows, err := SQLM.ExecuteSQL(query)
// 	if err != nil {
// 		http.Error(w, err.Error(), http.StatusBadRequest)
// 		return
// 	}
// 	w.Header().Set("Content-Type", "application/json")
// 	json.NewEncoder(w).Encode(
// 		struct {
// 			Rows []map[string]any `json:"rows"`
// 		}{Rows: rows},
// 	)
// }
