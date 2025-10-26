package main

import (
    "context"
    "flag"
    "log"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/hackohio/gateway/internal/gateway"
)

func getenv(key, def string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return def
}

func main() {
    // Config via flags or env
    var (
        addr          = flag.String("addr", getenv("GATEWAY_ADDR", ":8080"), "HTTP listen address")
        namespace     = flag.String("namespace", getenv("GATEWAY_NAMESPACE", "default"), "namespace to create NodeActions in")
        isName        = flag.String("instructionset", getenv("INSTRUCTIONSET_NAME", "wasmtime"), "InstructionSet name")
        instruction   = flag.String("instruction", getenv("INSTRUCTION_NAME", "run"), "Instruction name")
        runtime       = flag.String("runtime", getenv("RUNTIME", "exec"), "Instruction runtime (exec|grpc)")
        podSelector   = flag.String("agent-pod-selector", getenv("AGENT_POD_SELECTOR", "app=kuberisc-agent"), "label selector to find capable agent pods")
        verbose       = flag.Bool("v", getenv("GATEWAY_VERBOSE", "") != "", "verbose logging")
        graceStr      = flag.String("watch-grace", getenv("WATCH_GRACE", "500ms"), "extra grace on top of timeout for watch")
    )
    flag.Parse()

    grace, err := time.ParseDuration(*graceStr)
    if err != nil {
        log.Fatalf("invalid WATCH_GRACE: %v", err)
    }

    if !*verbose {
        log.SetFlags(0)
    }

    // Build server
    srv, err := gateway.NewServer(gateway.Config{
        Namespace:       *namespace,
        InstructionSet:  *isName,
        Instruction:     *instruction,
        Runtime:         *runtime,
        AgentPodSelector: *podSelector,
        WatchGrace:      grace,
        Logger:          log.Default(),
    })
    if err != nil {
        log.Fatalf("gateway init: %v", err)
    }

    mux := http.NewServeMux()
    mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); _, _ = w.Write([]byte("ok")) })
    mux.Handle("/v1/functions/", srv)

    httpSrv := &http.Server{Addr: *addr, Handler: mux}

    // Graceful shutdown on signal
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()
    go func() {
        <-ctx.Done()
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
        defer cancel()
        _ = httpSrv.Shutdown(shutdownCtx)
    }()

    log.Printf("gateway listening on %s (ns=%s is=%s/%s runtime=%s)", *addr, *namespace, *isName, *instruction, *runtime)
    if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        log.Fatalf("listen: %v", err)
    }
}

