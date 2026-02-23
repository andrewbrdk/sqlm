package main

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
	db *sql.DB
}

type Config struct {
	port     string
	dbFile   string
	password string
}

type Chat struct {
	Id      int       `json:"id"`
	Title   string    `json:"title"`
	Msgs    []*Msg    `json:"msgs"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

type Msg struct {
	Id       int       `json:"id"`
	ChatId   int       `json:"chat_id"`
	Position int       `json:"position"`
	Type     int       `json:"type"`
	Text     string    `json:"text"`
	Created  time.Time `json:"created"`
}

func main() {
	initConfig()
	jwtSecretKey = generateRandomKey(32)
	infoLog = log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	errorLog = log.New(os.Stdout, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
	SQLM.initDB()
	if _, err := SQLM.loadChats(); err != nil {
		errorLog.Printf("Failed to load chats: %v", err)
	}
	httpServer()
}

func initConfig() {
	CONF.port = ":8080"
	CONF.dbFile = "./sqlm.db"
	CONF.password = ""
	if port := os.Getenv("SQLM_PORT"); port != "" {
		CONF.port = ":" + port
	}
	if dbFile := os.Getenv("SQLM_DBFILE"); dbFile != "" {
		CONF.dbFile = dbFile
	}
	CONF.password = os.Getenv("SQLM_PASSWORD")
}

func (S *Sqlm) initDB() {
	var err error
	firstRun := false
	_, err = os.Stat(CONF.dbFile)
	if errors.Is(err, os.ErrNotExist) {
		firstRun = true
	}
	S.db, err = sql.Open("sqlite3", CONF.dbFile)
	if err != nil {
		log.Fatalf("cannot open sqlite db: %v", err)
	}
	_, err = S.db.Exec(`
        PRAGMA foreign_keys = ON;

        CREATE TABLE IF NOT EXISTS chats (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            title TEXT NOT NULL,
			created DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

        CREATE TABLE IF NOT EXISTS messages (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id INTEGER NOT NULL,
			position INTEGER NOT NULL DEFAULT 0,
			type INT NOT NULL DEFAULT 0,
			text TEXT NOT NULL,
			created DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(chat_id) REFERENCES chats(id) ON DELETE CASCADE,
			FOREIGN KEY(type) REFERENCES message_type(id)
        );

		CREATE INDEX IF NOT EXISTS idx_messages_chatid_position ON messages(chat_id, position);

		CREATE TABLE IF NOT EXISTS message_type (
			id INTEGER PRIMARY KEY,
			type TEXT NOT NULL
		);
		INSERT OR IGNORE INTO message_type (id, type) 
		VALUES (0, 'user'), (1, 'assistant');
    `)
	if err != nil {
		log.Fatalf("Can't create tables: %v", err)
	}
	if firstRun {
		infoLog.Println("Database created")
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

func (S *Sqlm) CreateChat(title string) (int, error) {
	infoLog.Printf("Creating chat '%s'", title)
	tx, err := S.db.Begin()
	if err != nil {
		errorLog.Printf("Failed to begin transaction: %v", err)
		return 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	res, _ := tx.Exec(`INSERT INTO chats(title) VALUES (?)`, title)
	newId, _ := res.LastInsertId()
	err = tx.Commit()
	if err != nil {
		errorLog.Printf("Commit failed: %v", err)
		return 0, err
	}
	return int(newId), nil
}

func (S *Sqlm) DeleteChat(id int) error {
	tx, err := S.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	tx.Exec(`DELETE FROM chats WHERE id = ?`, id)
	err = tx.Commit()
	if err != nil {
		errorLog.Printf("Commit failed during delete: %v", err)
		return err
	}
	return nil
}

func (S *Sqlm) RenameChat(id int, newTitle string) error {
	_, err := S.db.Exec(`
        UPDATE chats
        SET title = ?, updated = CURRENT_TIMESTAMP
        WHERE id = ?
    `, newTitle, id)
	if err != nil {
		errorLog.Printf("Failed to rename page id='%d': %v", id, err)
		return err
	}
	return nil
}

func (S *Sqlm) loadChats() ([]*Chat, error) {
	rows, err := S.db.Query(`
        SELECT 
			id, 
			title,
			created,
			updated
        FROM chats
		ORDER BY updated DESC`)
	if err != nil {
		errorLog.Printf("loadChats error: %v", err)
		return nil, err
	}
	defer rows.Close()

	chats := make([]*Chat, 0)
	for rows.Next() {
		chat := &Chat{}
		err := rows.Scan(&chat.Id, &chat.Title, &chat.Created, &chat.Updated)
		if err != nil {
			errorLog.Printf("loadChats scan error: %v", err)
			return nil, err
		}
		chats = append(chats, chat)
	}
	return chats, nil
}

func (S *Sqlm) loadMessages(chatId int) []*Msg {
	rows, err := S.db.Query(`
		SELECT
			id,
			chat_id,
			position,
			type,
			text,
			created
		FROM messages
		WHERE chat_id = ?
		ORDER BY position ASC`, chatId)
	if err != nil {
		errorLog.Printf("loadMessages error: %v", err)
		return nil
	}
	defer rows.Close()

	msgs := make([]*Msg, 0)
	for rows.Next() {
		msg := &Msg{}
		err := rows.Scan(&msg.Id, &msg.ChatId, &msg.Position, &msg.Type, &msg.Text, &msg.Created)
		if err != nil {
			errorLog.Printf("loadMessages scan error: %v", err)
			return nil
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func (S *Sqlm) CreateMessage(chatID int, msgType int, text string) error {
	tx, err := S.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	var chatExists int
	err = tx.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM chats WHERE id = ?)
	`, chatID).Scan(&chatExists)
	if chatExists == 0 {
		errorLog.Printf("Chat with id=%d not found", chatID)
		return errors.New("chat not found")
	}
	var nextPos int
	err = tx.QueryRow(`
		SELECT COALESCE(MAX(position), -1) + 1
		FROM messages
		WHERE chat_id = ?
	`, chatID).Scan(&nextPos)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		INSERT INTO messages (chat_id, position, type, text)
		VALUES (?, ?, ?, ?)
	`, chatID, nextPos, msgType, text)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		UPDATE chats
		SET updated = CURRENT_TIMESTAMP
		WHERE id = ?
	`, chatID)
	if err != nil {
		return err
	}

	err = tx.Commit()
	return err
}

type Response struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func httpServer() {
	http.HandleFunc("/", httpIndex)
	http.Handle("/style.css", http.FileServer(http.FS(embedded)))
	http.HandleFunc("/login", httpLogin)
	http.HandleFunc("/chats", httpChats)
	http.HandleFunc("/create", httpCreateChat)
	http.HandleFunc("/delete", httpDeleteChat)
	http.HandleFunc("/rename", httpRenameChat)
	http.HandleFunc("/chat", httpChat)
	http.HandleFunc("/message", httpCreateMessage)
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

func httpChats(w http.ResponseWriter, r *http.Request) {
	err, code, msg := httpCheckAuth(w, r)
	if err != nil {
		http.Error(w, msg, code)
		return
	}
	chats, err := SQLM.loadChats()
	if err != nil {
		errorLog.Printf("Failed to load chats: %v", err)
		http.Error(w, "Failed to load chats", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chats)
}

func httpChat(w http.ResponseWriter, r *http.Request) {
	err, code, msg := httpCheckAuth(w, r)
	if err != nil {
		http.Error(w, msg, code)
		return
	}
	idStr := r.URL.Query().Get("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	msgs := SQLM.loadMessages(id)
	if msgs == nil {
		http.Error(w, "Can't read messages", 500)
		return
	}
	ms, err := json.Marshal(msgs)
	if err != nil {
		http.Error(w, "Failed to marshal messages", 500)
		return
	}
	resp := struct {
		Msgs json.RawMessage `json:"msgs"`
	}{Msgs: ms}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func httpCreateChat(w http.ResponseWriter, r *http.Request) {
	err, code, msg := httpCheckAuth(w, r)
	if err != nil {
		http.Error(w, msg, code)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid JSON", 400)
		return
	}

	newChatId, err := SQLM.CreateChat(req.Title)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	chats, err := SQLM.loadChats()
	if err != nil {
		errorLog.Printf("Failed to load chats: %v", err)
		http.Error(w, "Failed to load chats", http.StatusInternalServerError)
		return
	}
	resp := struct {
		Chats     []*Chat `json:"chats"`
		NewChatId int     `json:"new_chat_id"`
	}{Chats: chats, NewChatId: newChatId}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func httpDeleteChat(w http.ResponseWriter, r *http.Request) {
	err, code, msg := httpCheckAuth(w, r)
	if err != nil {
		http.Error(w, msg, code)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Id int `json:"id"`
	}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid JSON", 400)
		return
	}
	err = SQLM.DeleteChat(req.Id)
	if err != nil {
		http.Error(w, "Error deleting chat", 400)
		return
	}
	chats, err := SQLM.loadChats()
	if err != nil {
		errorLog.Printf("Failed to load chats: %v", err)
		http.Error(w, "Failed to load chats", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chats)
}

func httpRenameChat(w http.ResponseWriter, r *http.Request) {
	err, code, msg := httpCheckAuth(w, r)
	if err != nil {
		http.Error(w, msg, code)
		return
	}
	var req struct {
		Id    int    `json:"id"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Id == 0 || req.Title == "" {
		http.Error(w, "Missing ID or Title", http.StatusBadRequest)
		return
	}
	err = SQLM.RenameChat(req.Id, req.Title)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	chats, err := SQLM.loadChats()
	if err != nil {
		errorLog.Printf("Failed to load chats: %v", err)
		http.Error(w, "Failed to load chats", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chats)
}

func httpCreateMessage(w http.ResponseWriter, r *http.Request) {
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
		ChatID int    `json:"chat_id"`
		Type   int    `json:"type"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.ChatID <= 0 || req.Text == "" {
		http.Error(w, "Missing or invalid chat_id/text", http.StatusBadRequest)
		return
	}
	if err := SQLM.CreateMessage(req.ChatID, req.Type, req.Text); err != nil {
		http.Error(w, "Failed to save message", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{
		Status:  "ok",
		Message: "saved",
	})
}
