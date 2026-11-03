package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type app struct {
	db *sql.DB
	origin string
}

type participant struct {
	ID string `json:"id"`
	Name string `json:"name"`
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" { log.Fatal("DATABASE_URL is required") }
	db, err := sql.Open("pgx", dsn)
	if err != nil { log.Fatal(err) }
	defer db.Close()
	db.SetMaxOpenConns(10)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil { log.Fatal(err) }
	if err := migrate(ctx, db); err != nil { log.Fatal(err) }

	a := &app{db: db, origin: os.Getenv("ALLOWED_ORIGIN")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("POST /api/rooms", a.createRoom)
	mux.HandleFunc("GET /api/rooms/{code}/participants", a.listParticipants)
	mux.HandleFunc("POST /api/rooms/{code}/participants", a.addParticipant)
	mux.HandleFunc("POST /api/rooms/{code}/draw", a.draw)
	mux.HandleFunc("GET /api/rooms/{code}/my-result", a.myResult)
	s := &http.Server{Addr: env("ADDR", ":8080"), Handler: a.cors(mux), ReadHeaderTimeout: 5*time.Second, ReadTimeout: 10*time.Second, WriteTimeout: 15*time.Second, IdleTimeout: 60*time.Second}
	log.Printf("Santa API listening on %s", s.Addr)
	log.Fatal(s.ListenAndServe())
}

func migrate(ctx context.Context, db *sql.DB) error {
	statements := []string{
		"CREATE TABLE IF NOT EXISTS rooms (id TEXT PRIMARY KEY, code TEXT NOT NULL UNIQUE, admin_hash TEXT NOT NULL, drawn_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())",
		"CREATE TABLE IF NOT EXISTS participants (id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, recipient_id TEXT REFERENCES participants(id), created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())",
		"CREATE INDEX IF NOT EXISTS participants_room_idx ON participants(room_id)",
	}
	for _, statement := range statements { if _, err := db.ExecContext(ctx, statement); err != nil { return err } }
	return nil
}

func (a *app) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := r.Header.Get("Origin")
		if o != "" && (a.origin == "" || o == a.origin) {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		}
		if r.Method == "OPTIONS" { w.WriteHeader(http.StatusNoContent); return }
		next.ServeHTTP(w, r)
	})
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	if err := a.db.PingContext(r.Context()); err != nil { fail(w, 503, "database unavailable"); return }
	jsonOut(w, 200, map[string]string{"status":"ok"})
}

func (a *app) createRoom(w http.ResponseWriter, r *http.Request) {
	id, err := token(16); if err != nil { fail(w, 500, "random generation failed"); return }
	admin, err := token(32); if err != nil { fail(w, 500, "random generation failed"); return }
	for i:=0; i<5; i++ {
		code, err := roomCode()
		if err != nil { fail(w, 500, "random generation failed"); return }
		_, err = a.db.ExecContext(r.Context(), "INSERT INTO rooms(id,code,admin_hash) VALUES($1,$2,$3)", id, code, hash(admin))
		if err == nil { jsonOut(w, 201, map[string]string{"id":id,"code":code,"admin_token":admin}); return }
	}
	fail(w, 500, "could not create room")
}

func (a *app) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	t := bearer(r)
	if t == "" { fail(w, 401, "admin token required"); return "", false }
	var id string
	err := a.db.QueryRowContext(r.Context(), "SELECT id FROM rooms WHERE code=$1 AND admin_hash=$2", strings.ToUpper(r.PathValue("code")), hash(t)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) { fail(w, 403, "invalid room code or admin token"); return "", false }
	if err != nil { fail(w, 500, "authorization failed"); return "", false }
	return id, true
}

func (a *app) listParticipants(w http.ResponseWriter, r *http.Request) {
	roomID, ok := a.authorize(w,r); if !ok { return }
	rows, err := a.db.QueryContext(r.Context(), "SELECT id,name FROM participants WHERE room_id=$1 ORDER BY created_at,id", roomID)
	if err != nil { fail(w,500,"could not load participants"); return }
	defer rows.Close()
	items := make([]participant,0)
	for rows.Next() { var p participant; if err:=rows.Scan(&p.ID,&p.Name); err!=nil { fail(w,500,"could not read participants"); return }; items=append(items,p) }
	if err:=rows.Err(); err!=nil { fail(w,500,"could not read participants"); return }
	jsonOut(w,200,map[string]any{"participants":items})
}

func (a *app) addParticipant(w http.ResponseWriter, r *http.Request) {
	roomID, ok := a.authorize(w,r); if !ok { return }
	r.Body = http.MaxBytesReader(w,r.Body,1<<20)
	var input struct { Name string `json:"name"` }
	dec:=json.NewDecoder(r.Body); dec.DisallowUnknownFields()
	if err:=dec.Decode(&input); err!=nil { fail(w,400,"invalid JSON"); return }
	input.Name=strings.TrimSpace(input.Name)
	if input.Name=="" || len([]rune(input.Name))>80 { fail(w,400,"name must contain 1–80 characters"); return }
	var drawn sql.NullTime
	if err:=a.db.QueryRowContext(r.Context(),"SELECT drawn_at FROM rooms WHERE id=$1",roomID).Scan(&drawn); err!=nil { fail(w,500,"could not read room"); return }
	if drawn.Valid { fail(w,409,"the draw has already happened"); return }
	id,err:=token(16); if err!=nil { fail(w,500,"random generation failed"); return }
	secret,err:=token(32); if err!=nil { fail(w,500,"random generation failed"); return }
	_,err=a.db.ExecContext(r.Context(),"INSERT INTO participants(id,room_id,name,token_hash) VALUES($1,$2,$3,$4)",id,roomID,input.Name,hash(secret))
	if err!=nil { fail(w,500,"could not save participant"); return }
	jsonOut(w,201,map[string]any{"participant":participant{ID:id,Name:input.Name},"participant_token":secret})
}

func (a *app) draw(w http.ResponseWriter, r *http.Request) {
	roomID,ok:=a.authorize(w,r); if !ok { return }
	tx,err:=a.db.BeginTx(r.Context(),&sql.TxOptions{Isolation:sql.LevelSerializable})
	if err!=nil { fail(w,500,"could not start draw"); return }
	defer tx.Rollback()
	var drawn sql.NullTime
	if err:=tx.QueryRowContext(r.Context(),"SELECT drawn_at FROM rooms WHERE id=$1 FOR UPDATE",roomID).Scan(&drawn); err!=nil { fail(w,404,"room not found"); return }
	if drawn.Valid { fail(w,409,"the draw has already happened"); return }
	rows,err:=tx.QueryContext(r.Context(),"SELECT id FROM participants WHERE room_id=$1 ORDER BY id",roomID)
	if err!=nil { fail(w,500,"could not load participants"); return }
	ids:=make([]string,0)
	for rows.Next() { var id string; if err:=rows.Scan(&id); err!=nil { rows.Close(); fail(w,500,"could not read participants"); return }; ids=append(ids,id) }
	if err:=rows.Err(); err!=nil { rows.Close(); fail(w,500,"could not read participants"); return }; rows.Close()
	if len(ids)<2 { fail(w,400,"at least two participants are required"); return }
	if err:=shuffle(ids); err!=nil { fail(w,500,"shuffle failed"); return }
	// A random cycle ensures nobody is assigned to themselves.
	for i,giver:=range ids {
		recipient:=ids[(i+1)%len(ids)]
		if _,err:=tx.ExecContext(r.Context(),"UPDATE participants SET recipient_id=$1 WHERE id=$2 AND room_id=$3",recipient,giver,roomID); err!=nil { fail(w,500,"could not save draw"); return }
	}
	if _,err:=tx.ExecContext(r.Context(),"UPDATE rooms SET drawn_at=NOW() WHERE id=$1",roomID); err!=nil { fail(w,500,"could not finalize draw"); return }
	if err:=tx.Commit(); err!=nil { fail(w,500,"could not commit draw"); return }
	jsonOut(w,200,map[string]string{"status":"drawn"})
}

func (a *app) myResult(w http.ResponseWriter, r *http.Request) {
	t:=bearer(r); if t=="" { fail(w,401,"participant token required"); return }
	var giver,recipient string
	err:=a.db.QueryRowContext(r.Context(),
		"SELECT p.name, q.name FROM participants p JOIN rooms rm ON rm.id=p.room_id JOIN participants q ON q.id=p.recipient_id WHERE rm.code=$1 AND p.token_hash=$2 AND rm.drawn_at IS NOT NULL",
		strings.ToUpper(r.PathValue("code")),hash(t)).Scan(&giver,&recipient)
	if errors.Is(err,sql.ErrNoRows) { fail(w,404,"result not found; check room code and token"); return }
	if err!=nil { fail(w,500,"could not load result"); return }
	jsonOut(w,200,map[string]string{"participant":giver,"recipient":recipient})
}

func bearer(r *http.Request) string {
	const prefix="Bearer "
	v:=r.Header.Get("Authorization")
	if !strings.HasPrefix(v,prefix) { return "" }
	return strings.TrimSpace(strings.TrimPrefix(v,prefix))
}
func token(n int)(string,error){ b:=make([]byte,n); if _,e:=rand.Read(b); e!=nil{return "",e}; return hex.EncodeToString(b),nil }
func roomCode()(string,error){ const alphabet="ABCDEFGHJKLMNPQRSTUVWXYZ23456789"; var b strings.Builder; for i:=0;i<6;i++ { n,e:=rand.Int(rand.Reader,big.NewInt(int64(len(alphabet)))); if e!=nil{return "",e}; b.WriteByte(alphabet[n.Int64()]) }; return b.String(),nil }
func hash(s string)string{ h:=sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func shuffle(v []string)error{ for i:=len(v)-1;i>0;i-- { n,e:=rand.Int(rand.Reader,big.NewInt(int64(i+1))); if e!=nil{return e}; j:=int(n.Int64()); v[i],v[j]=v[j],v[i] }; return nil }
func jsonOut(w http.ResponseWriter,status int,v any){ w.Header().Set("Content-Type","application/json; charset=utf-8"); w.WriteHeader(status); _=json.NewEncoder(w).Encode(v) }
func fail(w http.ResponseWriter,status int,message string){ jsonOut(w,status,map[string]string{"error":message}) }
func env(k,def string)string{ if v:=os.Getenv(k);v!=""{return v};return def }
