package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const PING_API_VERSION = "0.0.2"

type Server struct {
	Host string
	Port int

	CORSWhitelist         map[string]bool
	AllowedDefaultMethods string
}

type PingResponse struct {
	Status string `json:"status"`
}

type VersionResponse struct {
	Version string `json:"version"`
}

type NowResponse struct {
	ServerTime string `json:"server_time"`
}

// CORS middleware
func (server *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var allowedOrigin string
		if server.CORSWhitelist["*"] {
			allowedOrigin = "*"
		} else {
			origin := r.Header.Get("Origin")
			if _, ok := server.CORSWhitelist[origin]; ok {
				allowedOrigin = origin
			}
		}

		if allowedOrigin != "" {
			allowHeaders := "Content-Type"
			if allowedOrigin != "*" {
				allowHeaders += ", Authorization"
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				// Don't allow credentials for wildcard
			}
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Allow-Methods", server.AllowedDefaultMethods)
			// Credentials are cookies, authorization headers, or TLS client certificates
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Log access requests in proper format
func (server *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		var ip string
		var splitErr error
		realIp := r.Header["X-Real-Ip"]
		if len(realIp) == 0 {
			ip, _, splitErr = net.SplitHostPort(r.RemoteAddr)
			if splitErr != nil {
				http.Error(w, fmt.Sprintf("Unable to parse client IP: %s", r.RemoteAddr), http.StatusBadRequest)
				return
			}
		} else {
			ip = realIp[0]
		}

		log.Printf("%s %s %s", ip, r.Method, r.URL.Path)
	})
}

// Handle panic errors to prevent server shutdown
func (server *Server) panicMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recoverErr := recover(); recoverErr != nil {
				log.Println("recovered", recoverErr)
				http.Error(w, "Internal server error", 500)
			}
		}()

		// There will be a defer with panic handler in each next function
		next.ServeHTTP(w, r)
	})
}

func (server *Server) pingRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := PingResponse{Status: "ok"}
	json.NewEncoder(w).Encode(response)
}

func (server *Server) versionRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := VersionResponse{Version: PING_API_VERSION}
	json.NewEncoder(w).Encode(response)
}

func (server *Server) nowRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	serverTime := time.Now().Format("2006-01-02 15:04:05.999999-07")
	response := NowResponse{ServerTime: serverTime}
	json.NewEncoder(w).Encode(response)
}

func main() {
	shutdownTimeout := 5

	var hostF, corsWhitelistF string
	var portF, sReadTimeoutF, sWriteTimeoutF int
	flag.StringVar(&hostF, "host", "0.0.0.0", "Server host")
	flag.IntVar(&portF, "port", 9001, "Server port")
	flag.StringVar(&corsWhitelistF, "cors", "*", "List of comma separated domains for CORS")
	flag.IntVar(&sReadTimeoutF, "server-read-timeout", 10, "Server read timeout")
	flag.IntVar(&sWriteTimeoutF, "server-write-timeout", 10, "Server write timeout")

	flag.Parse()

	corsWhitelistRaw := strings.Split(strings.ReplaceAll(corsWhitelistF, " ", ""), ",")

	corsWhitelist := make(map[string]bool)
	for _, uri := range corsWhitelistRaw {
		if uri == "*" {
			corsWhitelist["*"] = true
			break
		}
		valid, err := url.ParseRequestURI(uri)
		if err != nil {
			log.Printf("Ignoring incorrect URI %s", uri)
			continue
		}
		corsWhitelist[valid.String()] = true
	}

	corsSlice := make([]string, 0, len(corsWhitelist))
	for k := range corsWhitelist {
		corsSlice = append(corsSlice, k)
	}

	server := Server{
		Host: hostF,
		Port: portF,

		CORSWhitelist:         corsWhitelist,
		AllowedDefaultMethods: "GET,OPTIONS",
	}

	serveMux := http.NewServeMux()
	serveMux.HandleFunc("/now", server.nowRoute)
	serveMux.HandleFunc("/ping", server.pingRoute)
	serveMux.HandleFunc("/version", server.versionRoute)

	// Set list of common middleware, from bottom to top
	commonHandler := server.corsMiddleware(serveMux)
	commonHandler = server.logMiddleware(commonHandler)
	commonHandler = server.panicMiddleware(commonHandler)

	s := http.Server{
		Addr:         fmt.Sprintf("%s:%d", server.Host, server.Port),
		Handler:      commonHandler,
		ReadTimeout:  time.Duration(sReadTimeoutF) * time.Second,
		WriteTimeout: time.Duration(sWriteTimeoutF) * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Starting ping HTTP server at %s:%d and whitelisted CORS %v", server.Host, server.Port, corsSlice)
		if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("Shutting down server gracefully in %d seconds...", shutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(shutdownTimeout)*time.Second)
	defer cancel()

	if err := s.Shutdown(shutdownCtx); err != nil {
		log.Printf("Graceful shutdown failed: %v", err)
		os.Exit(1)
	}

	log.Println("Server stopped")
}
