// Command fakellama imitates the parts of llama-server that Benchwarmer
// depends on: /health readiness after a load delay, and OpenAI-compatible
// chat completions with optional SSE streaming. It is a test and development
// fixture, not a product component.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"time"
)

func main() {
	host := flag.String("host", "127.0.0.1", "listen host")
	port := flag.Int("port", 8081, "listen port")
	model := flag.String("m", "fake.gguf", "model path (must exist unless -allow-missing-model)")
	allowMissing := flag.Bool("allow-missing-model", true, "do not require the model file to exist")
	loadDelay := flag.Duration("load-delay", 500*time.Millisecond, "time before /health reports ready")
	tokenDelay := flag.Duration("token-delay", 20*time.Millisecond, "delay between streamed tokens")
	tokens := flag.Int("tokens", 16, "tokens per completion")
	crashAfter := flag.Duration("crash-after", 0, "exit with status 3 after this long (0 = never)")
	spawnChild := flag.Bool("spawn-child", false, "spawn a sleeping child process (process-tree tests)")
	ignoreInterrupt := flag.Bool("ignore-interrupt", false, "ignore Ctrl+C / SIGINT (graceful-stop fallback tests)")
	// Accept and ignore common llama-server flags so real argument lists work.
	for _, f := range []string{"c", "ngl", "n-gpu-layers", "ctx-size", "t", "threads", "a", "alias", "np", "parallel", "fa", "flash-attn"} {
		flag.String(f, "", "ignored")
	}
	flag.Parse()

	if !*allowMissing {
		if _, err := os.Stat(*model); err != nil {
			log.Fatalf("model: %v", err)
		}
	}
	if *ignoreInterrupt {
		signal.Ignore(os.Interrupt)
	}
	if *spawnChild {
		spawnSleeper()
	}
	if *crashAfter > 0 {
		time.AfterFunc(*crashAfter, func() { log.Print("fakellama: simulated crash"); os.Exit(3) })
	}

	var ready atomic.Bool
	time.AfterFunc(*loadDelay, func() { ready.Store(true) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, *model)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, `{"error":"loading"}`, http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Stream    bool `json:"stream"`
			MaxTokens int  `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := *tokens
		if req.MaxTokens > 0 && req.MaxTokens < n {
			n = req.MaxTokens
		}
		if !req.Stream {
			time.Sleep(time.Duration(n) * *tokenDelay)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"fake","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"%s"},"finish_reason":"stop"}],"usage":{"completion_tokens":%d}}`, repeat("tok ", n), n)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < n; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(*tokenDelay):
			}
			fmt.Fprintf(w, "data: {\"id\":\"fake\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	ln, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fakellama listening on %s", ln.Addr())
	log.Fatal(http.Serve(ln, mux))
}

func repeat(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}
