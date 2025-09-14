package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/sessions"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"

	_ "github.com/mattn/go-sqlite3"
)

var (
	db       *sql.DB
	store    *sessions.CookieStore
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true // allo all origins for simplicity ad avoid errors
		},
	}
	sessionName = "chat-session"
	uploadsDir  = "uploads"
)

type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
	user User
}

type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	rooms      map[int]map[*Client]bool
	mu         sync.Mutex
}

type Message struct {
	Type        string    `json:"type"`
	Content     string    `json:"content"`
	Sender      User      `json:"sender"`
	RoomID      int       `json:"room_id,omitempty"`
	RecipientID int       `json:"recipient_id,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}

func newHub() *Hub {
	return &Hub{
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
		rooms:      make(map[int]map[*Client]bool),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			h.broadcastOnlineUsers()
		case client := <-h.unregister:
			h.mu.Lock()
			var removedRooms []int
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				for roomID := range h.rooms {
					if _, ok := h.rooms[roomID][client]; ok {
						delete(h.rooms[roomID], client)
						removedRooms = append(removedRooms, roomID)
					}
				}
				close(client.send)
			}
			h.mu.Unlock()
			h.broadcastOnlineUsers()
			for _, roomID := range removedRooms {
				h.broadcastRoomMembers(roomID)
			}
		case messageData := <-h.broadcast:
			var msg Message
			if err := json.Unmarshal(messageData, &msg); err != nil {
				log.Printf("Error unmarshalling broadcast message: %v", err)
				continue
			}

			h.mu.Lock()
			var targetClients []*Client
			if msg.Type == "direct_message" {
				for c := range h.clients {
					if c.user.ID == msg.Sender.ID || c.user.ID == msg.RecipientID {
						targetClients = append(targetClients, c)
					}
				}
			} else if msg.RoomID != 0 {
				if roomClients, ok := h.rooms[msg.RoomID]; ok {
					for c := range roomClients {
						targetClients = append(targetClients, c)
					}
				}
			}
			h.mu.Unlock()

			for _, client := range targetClients {
				select {
				case client.send <- messageData:
				default:
					h.mu.Lock()
					close(client.send)
					delete(h.clients, client)
					h.mu.Unlock()
				}
			}
		}
	}
}

func (h *Hub) broadcastOnlineUsers() {
	h.mu.Lock()
	var onlineUsers []User
	userSet := make(map[int]bool)
	var clientList []*Client
	for c := range h.clients {
		if !userSet[c.user.ID] {
			userSet[c.user.ID] = true
			onlineUsers = append(onlineUsers, c.user)
		}
		clientList = append(clientList, c)
	}
	payload, err := json.Marshal(map[string]interface{}{"type": "online_users", "users": onlineUsers})
	h.mu.Unlock()
	if err != nil {
		log.Printf("error marshalling online users: %v", err)
		return
	}

	for _, client := range clientList {
		select {
		case client.send <- payload:
		default:
			h.mu.Lock()
			close(client.send)
			delete(h.clients, client)
			h.mu.Unlock()
		}
	}
}

func (h *Hub) broadcastRoomMembers(roomID int) {
	h.mu.Lock()
	room, ok := h.rooms[roomID]
	if !ok {
		h.mu.Unlock()
		return
	}
	var memberUsers []User
	userSet := make(map[int]bool)
	var clientList []*Client
	for c := range room {
		if !userSet[c.user.ID] {
			userSet[c.user.ID] = true
			memberUsers = append(memberUsers, c.user)
		}
		clientList = append(clientList, c)
	}
	payload, err := json.Marshal(map[string]interface{}{"type": "room_members", "room_id": roomID, "users": memberUsers})
	h.mu.Unlock()
	if err != nil {
		log.Printf("error marshalling room members: %v", err)
		return
	}

	for _, client := range clientList {
		select {
		case client.send <- payload:
		default:
			h.mu.Lock()
			close(client.send)
			delete(h.clients, client)
			h.mu.Unlock()
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	for {
		_, messageData, err := c.conn.ReadMessage()
		if err != nil {
			if !websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Read error: %v", err)
			}
			break
		}

		var msg Message
		if err := json.Unmarshal(messageData, &msg); err != nil {
			log.Printf("Error unmarshalling incoming message: %v", err)
			continue
		}

		msg.Sender = c.user
		msg.Timestamp = time.Now()

		finalMsg, err := json.Marshal(msg)
		if err != nil {
			log.Printf("Error marshalling final message: %v", err)
			continue
		}

		if msg.Type == "message" || msg.Type == "direct_message" {
			if msg.Type == "message" {
				_, err = db.Exec("INSERT INTO messages (content, user_id, room_id) VALUES (?, ?, ?)", msg.Content, msg.Sender.ID, msg.RoomID)
			} else {
				_, err = db.Exec("INSERT INTO direct_messages (content, sender_id, recipient_id) VALUES (?, ?, ?)", msg.Content, msg.Sender.ID, msg.RecipientID)
			}
			if err != nil {
				log.Printf("Failed to save message to DB: %v", err)
			}
		}

		c.hub.broadcast <- finalMsg
	}
}

func (c *Client) writePump() {
	defer c.conn.Close()
	for message := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
			log.Printf("Write error: %v", err)
			return
		}
	}
}

func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, sessionName)
	userIDVal := session.Values["user_id"]
	if userIDVal == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}
	userID := userIDVal.(int)

	var user User
	err := db.QueryRow("SELECT id, username FROM users WHERE id = ?", userID).Scan(&user.ID, &user.Username)
	if err != nil {
		log.Printf("Could not find user for websocket: %v. Clearing invalid session.", err)
		session.Values["user_id"] = nil
		session.Options.MaxAge = -1
		session.Save(r, w)
		http.Error(w, "Invalid session", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	client := &Client{hub: hub, conn: conn, send: make(chan []byte, 256), user: user}
	client.hub.register <- client

	roomIDStr := r.URL.Query().Get("room_id")
	var roomToJoin int
	if roomIDStr != "" {
		var parseErr error
		roomToJoin, parseErr = strconv.Atoi(roomIDStr)
		if parseErr != nil {
			log.Printf("Invalid room_id: %s", roomIDStr)
		} else {
			var count int
			err = db.QueryRow("SELECT COUNT(*) FROM room_members WHERE user_id = ? AND room_id = ?", user.ID, roomToJoin).Scan(&count)
			if err != nil || count == 0 {
				log.Printf("User %d not a member of room %d", user.ID, roomToJoin)
			} else {
				hub.mu.Lock()
				if _, ok := hub.rooms[roomToJoin]; !ok {
					hub.rooms[roomToJoin] = make(map[*Client]bool)
				}
				hub.rooms[roomToJoin][client] = true
				hub.mu.Unlock()
				hub.broadcastRoomMembers(roomToJoin)
			}
		}
	}

	go client.writePump()
	go client.readPump()
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "./chat.db")
	if err != nil {
		log.Fatal(err)
	}

	schema := `
    CREATE TABLE IF NOT EXISTS users (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        username TEXT UNIQUE,
        password_hash TEXT
    );
    CREATE TABLE IF NOT EXISTS rooms (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name TEXT UNIQUE,
        description TEXT,
        is_private BOOLEAN,
        password_hash TEXT,
        created_by INTEGER,
        FOREIGN KEY(created_by) REFERENCES users(id)
    );
    CREATE TABLE IF NOT EXISTS messages (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        content TEXT,
        user_id INTEGER,
        room_id INTEGER,
        timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
        FOREIGN KEY(user_id) REFERENCES users(id),
        FOREIGN KEY(room_id) REFERENCES rooms(id)
    );
	CREATE TABLE IF NOT EXISTS room_members (
		user_id INTEGER,
		room_id INTEGER,
		PRIMARY KEY (user_id, room_id),
		FOREIGN KEY(user_id) REFERENCES users(id),
		FOREIGN KEY(room_id) REFERENCES rooms(id)
	);
	CREATE TABLE IF NOT EXISTS direct_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		content TEXT,
		sender_id INTEGER,
		recipient_id INTEGER,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(sender_id) REFERENCES users(id),
		FOREIGN KEY(recipient_id) REFERENCES users(id)
	);
    `
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}

	generalRoomSQL := `
	INSERT OR IGNORE INTO rooms (id, name, description, is_private, created_by) 
	VALUES (1, 'General', 'A place for everyone to chat', 0, NULL);
	`
	if _, err := db.Exec(generalRoomSQL); err != nil {
		log.Printf("Could not create default General room: %v", err)
	}
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(creds.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec("INSERT INTO users (username, password_hash) VALUES (?, ?)", creds.Username, string(hashedPassword))
	if err != nil {
		http.Error(w, "Username already exists", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	var user User
	var hashedPassword string
	err := db.QueryRow("SELECT id, username, password_hash FROM users WHERE username = ?", creds.Username).Scan(&user.ID, &user.Username, &hashedPassword)
	if err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(creds.Password)); err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	session, _ := store.Get(r, sessionName)
	session.Values["user_id"] = user.ID
	session.Values["username"] = user.Username
	session.Save(r, w)

	_, err = db.Exec("INSERT OR IGNORE INTO room_members (user_id, room_id) VALUES (?, ?)", user.ID, 1)
	if err != nil {
		log.Printf("Failed to add user %d to General room on login: %v", user.ID, err)
	}

	json.NewEncoder(w).Encode(user)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, sessionName)
	session.Values["user_id"] = nil
	session.Options.MaxAge = -1
	session.Save(r, w)
	w.WriteHeader(http.StatusOK)
}

func handleGetCurrentUser(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, sessionName)
	userID := session.Values["user_id"]
	username := session.Values["username"]
	if userID == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}
	json.NewEncoder(w).Encode(User{ID: userID.(int), Username: username.(string)})
}

func handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		IsPrivate   bool   `json:"is_private"`
		Password    string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	session, _ := store.Get(r, sessionName)
	userIDVal, ok := session.Values["user_id"]
	if !ok || userIDVal == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}
	userID, ok := userIDVal.(int)
	if !ok {
		http.Error(w, "Invalid user ID in session", http.StatusInternalServerError)
		return
	}

	var passwordHash sql.NullString
	if req.IsPrivate && req.Password != "" {
		hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "Server error hashing password", http.StatusInternalServerError)
			return
		}
		passwordHash.String = string(hashed)
		passwordHash.Valid = true
	}

	res, err := db.Exec(`INSERT INTO rooms (name, description, is_private, password_hash, created_by) VALUES (?, ?, ?, ?, ?)`,
		req.Name, req.Description, req.IsPrivate, passwordHash, userID)
	if err != nil {
		log.Printf("ERROR: Failed to create room in database: %v", err)
		if strings.Contains(err.Error(), "UNIQUE constraint failed: rooms.name") {
			http.Error(w, "A room with this name already exists.", http.StatusConflict)
		} else {
			http.Error(w, "A database error occurred. (Check server logs for details).", http.StatusInternalServerError)
		}
		return
	}

	roomID, _ := res.LastInsertId()
	_, err = db.Exec(`INSERT INTO room_members (user_id, room_id) VALUES (?, ?)`, userID, roomID)
	if err != nil {
		log.Printf("Failed to add creator to room members: %v", err)
	}

	w.WriteHeader(http.StatusCreated)
}

func handleGetRooms(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, sessionName)
	userID := session.Values["user_id"]

	rows, err := db.Query(`
		SELECT r.id, r.name, r.description, COUNT(m.user_id) as member_count, r.is_private
		FROM rooms r
		LEFT JOIN room_members m ON r.id = m.room_id
		GROUP BY r.id
	`)
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	publicRooms := []map[string]interface{}{}
	for rows.Next() {
		var id, memberCount int
		var name, description string
		var isPrivate bool
		rows.Scan(&id, &name, &description, &memberCount, &isPrivate)
		publicRooms = append(publicRooms, map[string]interface{}{
			"id": id, "name": name, "description": description, "member_count": memberCount, "is_private": isPrivate,
		})
	}

	memberRooms := []map[string]interface{}{}
	if userID != nil {
		rows, err := db.Query(`
			SELECT r.id, r.name FROM rooms r
			JOIN room_members m ON r.id = m.room_id
			WHERE m.user_id = ?`, userID.(int))
		if err != nil {
			http.Error(w, "Server error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id int
			var name string
			rows.Scan(&id, &name)
			memberRooms = append(memberRooms, map[string]interface{}{
				"id": id, "name": name,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"public_rooms": publicRooms,
		"member_rooms": memberRooms,
	})
}

func handleJoinRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RoomID   int    `json:"room_id"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	session, _ := store.Get(r, sessionName)
	userID := session.Values["user_id"].(int)

	var isPrivate bool
	var passwordHash sql.NullString
	err := db.QueryRow("SELECT is_private, password_hash FROM rooms WHERE id = ?", req.RoomID).Scan(&isPrivate, &passwordHash)
	if err != nil {
		http.Error(w, "Room not found", http.StatusNotFound)
		return
	}

	if isPrivate {
		if !passwordHash.Valid {
			http.Error(w, "This is a private, invite-only room", http.StatusForbidden)
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(passwordHash.String), []byte(req.Password)); err != nil {
			http.Error(w, "Incorrect password", http.StatusForbidden)
			return
		}
	}

	_, err = db.Exec("INSERT OR IGNORE INTO room_members (user_id, room_id) VALUES (?, ?)", userID, req.RoomID)
	if err != nil {
		http.Error(w, "Failed to join room", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

func handleGetMessages(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room_id")
	otherUserID := r.URL.Query().Get("user_id") // For DMs
	session, _ := store.Get(r, sessionName)
	currentUserID := session.Values["user_id"].(int)

	var rows *sql.Rows
	var err error

	if roomID != "" {
		rows, err = db.Query(`
			SELECT m.content, u.id, u.username, m.timestamp
			FROM messages m JOIN users u ON m.user_id = u.id
			WHERE m.room_id = ? ORDER BY m.timestamp ASC
		`, roomID)
	} else if otherUserID != "" {
		rows, err = db.Query(`
			SELECT dm.content, u.id, u.username, dm.timestamp
			FROM direct_messages dm JOIN users u ON dm.sender_id = u.id
			WHERE (dm.sender_id = ? AND dm.recipient_id = ?) OR (dm.sender_id = ? AND dm.recipient_id = ?)
			ORDER BY dm.timestamp ASC
		`, currentUserID, otherUserID, otherUserID, currentUserID)
	} else {
		http.Error(w, "Missing room_id or user_id", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	messages := []Message{}
	for rows.Next() {
		var msg Message
		rows.Scan(&msg.Content, &msg.Sender.ID, &msg.Sender.Username, &msg.Timestamp)
		messages = append(messages, msg)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messages)
}

func main() {
	initDB()
	store = sessions.NewCookieStore([]byte("a-very-secret-key"))
	if _, err := os.Stat(uploadsDir); os.IsNotExist(err) {
		os.Mkdir(uploadsDir, 0755)
	}

	hub := newHub()
	go hub.run()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/register", handleRegister)
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/current_user", handleGetCurrentUser)

	mux.HandleFunc("/api/rooms", handleGetRooms)
	mux.HandleFunc("/api/rooms/create", handleCreateRoom)
	mux.HandleFunc("/api/rooms/join", handleJoinRoom)

	mux.HandleFunc("/api/messages", handleGetMessages)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		session, _ := store.Get(r, sessionName)
		if session.Values["user_id"] == nil {
			http.Error(w, "Not authenticated", http.StatusUnauthorized)
			return
		}

		r.ParseMultipartForm(10 << 20) // 10 MB limit

		file, handler, err := r.FormFile("file")
		if err != nil {
			log.Printf("Error retrieving file from form: %v", err)
			http.Error(w, "Error retrieving file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		safeFilename := time.Now().Format("20060102150405") + "_" + filepath.Base(handler.Filename)
		dstPath := filepath.Join(uploadsDir, safeFilename)

		dst, err := os.Create(dstPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		if _, err := io.Copy(dst, file); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": "/uploads/" + safeFilename, "filename": handler.Filename})
	})

	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(uploadsDir))))

	log.Println("Server starting on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
