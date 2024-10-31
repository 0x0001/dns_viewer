package main

import (
	"embed"
	"encoding/binary"
	"encoding/json"
	"flag"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "embed"

	"github.com/miekg/dns"
	"go.etcd.io/bbolt"
)

type logRecord struct {
	Name      string `json:"name"`
	Type      uint16 `json:"type"`
	RemoteIP  string `json:"remote_ip"`
	Timestamp int64  `json:"timestamp"`
}

func main() {
	port := flag.Int("port", 8053, "port to run on")
	upstream := flag.String("upstream", "", "upstream DNS server")
	flag.Parse()

	if *upstream == "" {
		log.Println("Upstream DNS server not specified, using system default")
		cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			log.Fatalf("Upstream DNS server not specified and Failed to load resolv.conf: %s\n", err.Error())
		}
		upstream = &cfg.Servers[0]
	}
	log.Println("Upstream DNS server:", *upstream)
	r, err := dns.Dial("udp", *upstream+":53")
	if err != nil {
		log.Fatalf("Failed to dial upstream DNS server: %s\n", err.Error())
	}

	db, err := bbolt.Open("dns.db", 0600, nil)
	if err != nil {
		log.Fatalf("Failed to open database: %s\n", err.Error())
	}
	defer db.Close()
	db.Update(func(tx *bbolt.Tx) error {
		for _, name := range []string{
			"logs", "metrics",
		} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	})

	logChan := make(chan logRecord, 1000)
	go processLogs(db, logChan)

	dns.HandleFunc(".", func(w dns.ResponseWriter, m *dns.Msg) {
		for _, q := range m.Question {
			log.Println("Question:", q.Name, dns.TypeToString[q.Qtype])
			logChan <- logRecord{
				Name:      q.Name,
				Type:      q.Qtype,
				RemoteIP:  w.RemoteAddr().String(),
				Timestamp: time.Now().Unix(),
			}
		}
		if err := r.WriteMsg(m); err != nil {
			log.Printf("Failed to write message: %s\n", err.Error())
		}
		m, err = r.ReadMsg()
		if err != nil {
			log.Printf("Failed to read message: %s\n", err.Error())
		}
		// log.Printf("Received DNS response: %#v\n", m)
		w.WriteMsg(m)
		// w.WriteMsg(m)
	})

	go func() {
		srv := &dns.Server{Addr: ":" + strconv.Itoa(*port), Net: "udp"}
		log.Println("Starting DNS server on port", *port)
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("Failed to set udp listener %s\n", err.Error())
		}
	}()

	http.HandleFunc("/", handleShowLogs(db))

	go func() {
		log.Println("Starting HTTP server on port ", *port+1)
		http.ListenAndServe(":"+strconv.Itoa(*port+1), nil)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Fatalf("Signal (%v) received, stopping\n", s)
}

//go:embed index.html
var indexHTML []byte

//go:embed static
var staticFS embed.FS

func handleShowLogs(db *bbolt.DB) func(http.ResponseWriter, *http.Request) {
	tpl := template.Must(template.New("index.html").Funcs(template.FuncMap{
		"Atype": func(atype uint16) string {
			return dns.TypeToString[atype]
		},
		"FormatTimestamp": func(ts int64) string {
			return time.Unix(ts, 0).Format(time.RFC3339)
		},
	}).Parse(string(indexHTML)))
	fileServer := http.FileServer(http.FS(staticFS))

	return func(w http.ResponseWriter, r *http.Request) {

		if strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Add("Cache-Control", "public, max-age=31536000")
			fileServer.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html")

		type record struct {
			logRecord
			Sequence uint64 `json:"sequence"`
		}

		var records []record
		lastID := r.URL.Query().Get("last")
		limit := r.URL.Query().Get("limit")
		limitInt, err := strconv.Atoi(limit)

		const LIMIT_MAX = 100
		const DEFAULT_LIMIT = 20

		if err != nil {
			limitInt = DEFAULT_LIMIT
		}
		limitInt = max(1, limitInt)
		limitInt = min(LIMIT_MAX, limitInt)

		var seq uint64 = math.MaxUint64
		if lastID != "" {
			var err error
			seq, err = strconv.ParseUint(lastID, 10, 64)
			if err != nil {
				// http.Error(w, "Invalid last ID", http.StatusBadRequest)
				// return
				log.Println("Invalid last ID:", lastID)
				lastID = ""
			}
		}

		db.View(func(tx *bbolt.Tx) error {
			b := tx.Bucket([]byte("logs"))
			if b == nil {
				return nil
			}
			c := b.Cursor()
			count := 0
			c.Seek(binary.BigEndian.AppendUint64(nil, seq))
			for k, v := c.Prev(); k != nil; k, v = c.Prev() {
				var rec logRecord
				if err := json.Unmarshal(v, &rec); err != nil {
					log.Printf("Failed to unmarshal record: %s\n", err.Error())
					continue
				}
				seq := binary.BigEndian.Uint64(k)
				records = append(records, record{
					logRecord: rec,
					Sequence:  seq,
				})
				count++
				if count >= limitInt {
					break
				}
			}
			return nil
		})
		var nextID uint64
		if len(records) > 0 {
			nextID = records[len(records)-1].Sequence
		}
		// json.NewEncoder(w).Encode(records)
		if err := tpl.Execute(w, map[string]any{"records": records,
			"last":  nextID,
			"prev":  seq + uint64(limitInt),
			"limit": limitInt}); err != nil {
			log.Printf("Failed to execute template: %s\n", err.Error())
			http.Error(w, "Failed to execute template", http.StatusInternalServerError)
			return
		}
	}
}

func processLogs(db *bbolt.DB, logChan <-chan logRecord) {

	ticker := time.NewTicker(1 * time.Second)
	var tx *bbolt.Tx
	for {
		select {
		case rec := <-logChan:
			if tx == nil {
				var err error
				tx, err = db.Begin(true)
				if err != nil {
					log.Printf("Failed to begin transaction: %s\n", err.Error())
					continue
				}
			}
			// log.Println("Received log record:", rec)
			bucker := tx.Bucket([]byte("logs"))
			if bucker == nil {
				continue
			}
			seq, err := bucker.NextSequence()
			if err != nil {
				log.Printf("Failed to get next sequence: %s\n", err.Error())
				continue
			}
			key := binary.BigEndian.AppendUint64(nil, seq)
			value, err := json.Marshal(rec)
			if err != nil {
				log.Printf("Failed to marshal record: %s\n", err.Error())
				continue
			}
			if err := bucker.Put(key, value); err != nil {
				log.Printf("Failed to put record: %s\n", err.Error())
				continue
			}
		case <-ticker.C:
			if tx != nil {
				if err := tx.Commit(); err != nil {
					log.Printf("Failed to commit transaction: %s\n", err.Error())
				}
				tx = nil
			}
		}
	}
}
