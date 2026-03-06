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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/slack-go/slack"
)

//go:embed index.html style.css
var embedded embed.FS

var jwtSecretKey []byte

var infoLog *log.Logger
var errorLog *log.Logger

var CONF Config
var QUERYAGENT Queryagent

type Queryagent struct {
	execConnPool *pgxpool.Pool
}

type Config struct {
	port               string
	password           string
	openRouterKey      string
	openRouterModel    string
	execDB             string
	logFile            string
	contextPath        string
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
	QUERYAGENT.initExecConnPool()
	if QUERYAGENT.execConnPool != nil {
		defer QUERYAGENT.execConnPool.Close()
	}
	httpServer()
}

func initConfig() {
	CONF.port = ":8080"
	CONF.password = ""
	if port := os.Getenv("QUERYAGENT_PORT"); port != "" {
		CONF.port = ":" + port
	}
	CONF.password = os.Getenv("QUERYAGENT_PASSWORD")
	CONF.openRouterKey = strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	CONF.openRouterModel = strings.TrimSpace(os.Getenv("OPENROUTER_MODEL"))
	if CONF.openRouterKey == "" || CONF.openRouterModel == "" {
		log.Fatal("OPENROUTER_API_KEY and OPENROUTER_MODEL are required")
	}
	CONF.execDB = os.Getenv("QUERYAGENT_EXEC_DB")
	if CONF.execDB == "" {
		errorLog.Printf("QUERYAGENT_EXEC_DB is not set. SQL execution is not available.")
	}
	CONF.logFile = strings.TrimSpace(os.Getenv("QUERYAGENT_LOG_FILE"))
	if CONF.logFile == "" {
		errorLog.Printf("QUERYAGENT_LOG_FILE is not set. Logging is disabled.")
	}
	CONF.contextPath = os.Getenv("QUERYAGENT_CONTEXT_PATH")
	if CONF.contextPath == "" {
		errorLog.Printf("No context directory configured.")
	}
	CONF.slackSigningSecret = os.Getenv("QUERYAGENT_SLACK_SIGNING_SECRET")
	if CONF.slackSigningSecret == "" {
		errorLog.Printf("QUERYAGENT_SLACK_SIGNING_SECRET is not set.")
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

func (Q *Queryagent) initExecConnPool() {
	if strings.TrimSpace(CONF.execDB) == "" {
		infoLog.Printf("No execution database configured.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, CONF.execDB)
	if err != nil {
		log.Fatalf("cannot open execution db connection: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("cannot ping execution db connection: %v", err)
	}
	Q.execConnPool = pool
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
	contextMessages := loadContext(CONF.contextPath)
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

func loadContext(contextPath string) []string {
	if contextPath == "" {
		return []string{}
	}
	info, err := os.Stat(contextPath)
	if err != nil {
		infoLog.Printf("Context path not found or not readable: %s", contextPath)
		return []string{}
	}
	if !info.IsDir() {
		data, err := os.ReadFile(contextPath)
		if err != nil {
			errorLog.Printf("Failed to read context file %s: %v", contextPath, err)
			return []string{}
		}
		infoLog.Printf("Loaded context from %s", contextPath)
		return []string{string(data)}
	}
	entries, err := os.ReadDir(contextPath)
	if err != nil {
		infoLog.Printf("Context directory not found or not readable: %s", contextPath)
		return []string{}
	}
	var messages []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filePath := filepath.Join(contextPath, entry.Name())
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

func formatSQL(sql string) string {
	//todo: use something other than pg_format
	_, err := exec.LookPath("pg_format")
	if err != nil {
		errorLog.Printf("pg_format not found in PATH: %v", err)
		return strings.TrimSpace(sql)
	}
	cmd := exec.Command("pg_format", "-")
	cmd.Stdin = strings.NewReader(sql)
	output, err := cmd.CombinedOutput()
	if err != nil {
		errorLog.Printf("pg_format failed: %v: %s", err, string(output))
		return strings.TrimSpace(sql)
	}
	formatted := strings.TrimSpace(string(output))
	return formatted
}

type SQLResult struct {
	ColumnNames []string `json:"column_names"`
	ColumnTypes []string `json:"column_types"`
	Rows        [][]any  `json:"rows"`
	Truncated   bool     `json:"truncated"`
}

func (Q *Queryagent) ExecuteSQL(query string) (SQLResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return SQLResult{}, errors.New("Empty query.")
	}
	if Q.execConnPool == nil {
		return SQLResult{}, errors.New("Execution DB is not set.")
	}
	//todo: set timeout from config
	//todo: pass user context for cancellation
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := Q.execConnPool.Acquire(ctx)
	if err != nil {
		return SQLResult{}, err
	}
	defer conn.Release()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return SQLResult{}, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, query)
	if err != nil {
		return SQLResult{}, err
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	var result SQLResult
	for _, fd := range fds {
		result.ColumnNames = append(result.ColumnNames, string(fd.Name))
		dt, _ := conn.Conn().TypeMap().TypeForOID(uint32(fd.DataTypeOID))
		result.ColumnTypes = append(result.ColumnTypes, dt.Name)
	}

	//todo: deal with large results.
	const maxRows = 30
	for rows.Next() {
		if len(result.Rows) >= maxRows {
			result.Truncated = true
			rows.Close()
			break
		}
		values, err := rows.Values()
		if err != nil {
			return SQLResult{}, err
		}
		row := make([]any, len(values))
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				row[i] = string(b)
			} else {
				row[i] = v
			}
		}
		result.Rows = append(result.Rows, row)
	}

	if err := rows.Err(); err != nil {
		return SQLResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SQLResult{}, err
	}
	return result, nil
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
	http.HandleFunc("/fix", httpFixQuery)
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
	sql := formatSQL(parsed.SQL)
	//todo: validate SQL
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

func httpFixQuery(w http.ResponseWriter, r *http.Request) {
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
		Text  string `json:"text"`
		SQL   string `json:"sql"`
		Error string `json:"error"`
	}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	//todo: don't include error message if empty.
	//todo: change prompt?
	fixPrompt := fmt.Sprintf(
		`Fix the SQL below. Make sure it answers the original request. 
		SQL:
		 	%s
		
		Error message:
		 	%s
		 
		The original request:
		    %s
		`,
		strings.TrimSpace(req.SQL),
		strings.TrimSpace(req.Error),
		strings.TrimSpace(req.Text),
	)
	msgs := buildLLMMessages(fixPrompt)
	assistantText, err := callOpenRouter(msgs)
	if err != nil {
		errorLog.Printf("OpenRouter fix request failed: %v", err)
		http.Error(w, "Assistant unavailable", http.StatusBadGateway)
		return
	}
	var parsed struct {
		Outline string `json:"outline"`
		SQL     string `json:"sql"`
	}
	err = json.Unmarshal([]byte(assistantText), &parsed)
	if err != nil {
		errorLog.Printf("Failed to parse assistant fix response: %v", err)
		http.Error(w, "Assistant returned invalid JSON", http.StatusBadGateway)
		return
	}
	outline := strings.TrimSpace(parsed.Outline)
	sql := formatSQL(parsed.SQL)
	go logLLM(LLMLogEntry{
		ID:        generateUniqueID(),
		Timestamp: time.Now(),
		UserText:  fixPrompt,
		Outline:   outline,
		SQL:       sql,
		Context:   msgs,
	})
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
	result, err := QUERYAGENT.ExecuteSQL(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
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
			"text": "Usage: `/queryagent select * from users`",
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
		sql := formatSQL(parsed.SQL)
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
