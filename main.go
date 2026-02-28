package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/slack-go/slack"
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
	port               string
	password           string
	openRouterKey      string
	openRouterModel    string
	execDB             string
	logFile            string
	contextDir         string
	slackSigningSecret string
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

type LLMLogEntry struct {
	ID        string       `json:"id"`
	Timestamp time.Time    `json:"timestamp"`
	UserText  string       `json:"user_text"`
	Outline   string       `json:"outline"`
	SQL       string       `json:"sql"`
	Context   []LLMMessage `json:"context"`
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
	CONF.logFile = strings.TrimSpace(os.Getenv("SQLM_LOG_FILE"))
	if CONF.logFile == "" {
		errorLog.Printf("SQLM_LOG_FILE is not set. Logging is disabled.")
	}
	CONF.contextDir = os.Getenv("SQLM_CONTEXT_DIR")
	if CONF.contextDir == "" {
		errorLog.Printf("No context directory configured.")
	}
	CONF.slackSigningSecret = os.Getenv("SQLM_SLACK_SIGNING_SECRET")
	if CONF.slackSigningSecret == "" {
		errorLog.Printf("SQLM_SLACK_SIGNING_SECRET is not set.")
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
	contextMessages := loadContext(CONF.contextDir)
	for _, contextMsg := range contextMessages {
		out = append(out, LLMMessage{
			Role:    "system",
			Content: contextMsg,
		})
	}
	out = append(out, LLMMessage{
		Role:    "user",
		Content: msg,
	})
	return out
}

func loadContext(dirPath string) []string {
	if dirPath == "" {
		return []string{}
	}
	//todo: search
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		infoLog.Printf("Context directory not found or not readable: %s", dirPath)
		return []string{}
	}
	var messages []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filePath := filepath.Join(dirPath, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			errorLog.Printf("Failed to read context file %s: %v", filePath, err)
			continue
		}
		messages = append(messages, string(data))
		infoLog.Printf("Loaded context from %s", entry.Name())
	}
	return messages
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

func (S *Sqlm) ExecuteSQL(query string) ([]map[string]any, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty query")
	}
	if S.execConn == nil {
		return nil, errors.New("execution db not initialized")
	}
	//todo: set timeout from config
	//todo: pass user context for cancellation
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := S.execConn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	//todo: deal with large results.
	const maxRows = 30
	results := make([]map[string]any, 0, maxRows)
	for rows.Next() {
		if len(results) >= maxRows {
			break
		}
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(values))
		for i, fd := range fds {
			v := values[i]
			if b, ok := v.([]byte); ok {
				row[string(fd.Name)] = string(b)
			} else {
				row[string(fd.Name)] = v
			}
		}
		results = append(results, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return results, nil
}

func logLLM(entry LLMLogEntry) error {
	if strings.TrimSpace(CONF.logFile) == "" {
		return errors.New("log file not configured")
	}
	f, err := os.OpenFile(CONF.logFile,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		errorLog.Printf("unable to open log file %s: %v", CONF.logFile, err)
		return err
	}
	defer f.Close()
	b, err := json.Marshal(entry)
	if err != nil {
		errorLog.Printf("failed to marshal LLM log entry: %v", err)
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		errorLog.Printf("failed to write LLM log entry: %v", err)
		return err
	}
	return nil
}

func generateUniqueID() string {
	b := make([]byte, 8)
	_, err := rand.Read(b)
	if err != nil {
		errorLog.Printf("Failed to generate unique ID: %v", err)
		return ""
	}
	return hex.EncodeToString(b)
}

func httpServer() {
	http.HandleFunc("/", httpIndex)
	http.Handle("/style.css", http.FileServer(http.FS(embedded)))
	http.HandleFunc("/login", httpLogin)
	http.HandleFunc("/checkauth", httpCheckAuthHandler)
	http.HandleFunc("/message", httpUserMessage)
	http.HandleFunc("/execute", httpExecute)
	http.HandleFunc("/slack/slash", handleSlackSlash)
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
	msgs := buildLLMMessages(req.Text)
	assistantText, err := callOpenRouter(msgs)
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
	go logLLM(LLMLogEntry{
		ID:        generateUniqueID(),
		Timestamp: time.Now(),
		UserText:  req.Text,
		Outline:   outline,
		SQL:       sql,
		Context:   msgs,
	})
	//todo: log on failure
	//todo: error is lost
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"outline": outline,
		"sql":     sql,
	})
}

func httpExecute(w http.ResponseWriter, r *http.Request) {
	err, code, msg := httpCheckAuth(w, r)
	if err != nil {
		http.Error(w, msg, code)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	//todo: pass msg id
	var m struct {
		SQL string `json:"sql"`
	}
	err = json.NewDecoder(r.Body).Decode(&m)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(m.SQL)
	if query == "" {
		http.Error(w, "No SQL found on this message", http.StatusBadRequest)
		return
	}
	//todo: create cancel context
	rows, err := SQLM.ExecuteSQL(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(
		struct {
			Rows []map[string]any `json:"rows"`
		}{Rows: rows},
	)
}

func handleSlackSlash(w http.ResponseWriter, r *http.Request) {
	if !verifySlackSignature(r) {
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}
	cmd, err := slack.SlashCommandParse(r)
	if err != nil {
		errorLog.Printf("Failed to parse Slack slash command: %v", err)
		http.Error(w, "Failed to parse command", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	text := strings.TrimSpace(cmd.Text)
	if text == "" {
		json.NewEncoder(w).Encode(map[string]string{
			"text": "Usage: `/sqlm select * from users`",
		})
		return
	}
	w.Write([]byte(`{"response_type":"in_channel", "text":"Generating SQL..."}`))

	//todo: define func
	go func(cmd slack.SlashCommand, userText string) {
		msgs := buildLLMMessages(text)
		assistantText, err := callOpenRouter(msgs)
		if err != nil {
			errorLog.Printf("Slack, OpenRouter request failed: %v", err)
			json.NewEncoder(w).Encode(map[string]string{
				"text": fmt.Sprintf("Error: %v", err),
			})
			return
		}
		var parsed struct {
			Outline string `json:"outline"`
			SQL     string `json:"sql"`
		}
		err = json.Unmarshal([]byte(assistantText), &parsed)
		if err != nil {
			errorLog.Printf("Slack, Failed to parse assistant response: %v", err)
			json.NewEncoder(w).Encode(map[string]string{
				"text": fmt.Sprintf("Error: %v", err),
			})
			return
		}
		outline := strings.TrimSpace(parsed.Outline)
		sql := strings.TrimSpace(parsed.SQL)
		//todo: format and validate SQL
		go logLLM(LLMLogEntry{
			ID:        generateUniqueID(),
			Timestamp: time.Now(),
			UserText:  text,
			Outline:   outline,
			SQL:       sql,
			Context:   msgs,
		})
		postToResponseURL(cmd.ResponseURL, "```\n"+sql+"\n```")
	}(cmd, text)
	infoLog.Printf("Slack slash: query=%s", text)
}

func verifySlackSignature(r *http.Request) bool {
	if CONF.slackSigningSecret == "" {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	sv, err := slack.NewSecretsVerifier(r.Header, CONF.slackSigningSecret)
	if err != nil {
		return false
	}
	if _, err = sv.Write(body); err != nil {
		return false
	}
	return sv.Ensure() == nil
}

func postToResponseURL(responseURL, message string) {
	payload := map[string]interface{}{
		"replace_original": true,
		"response_type":    "in_channel",
		"text":             message,
	}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", responseURL, bytes.NewBuffer(data))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	_, err := client.Do(req)
	if err != nil {
		errorLog.Printf("Failed to replace Slack response: %v", err)
	}
}
