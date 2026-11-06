package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/go-redis/redis/v8"
	"github.com/joho/godotenv"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// App struct para injeção de dependência
type App struct {
	RedisClient         *redis.Client
	SqsSvc              SQSClient
	SqsQueueURL         string
	HttpClient          *http.Client
	FlagServiceURL      string
	TargetingServiceURL string
	Metrics             *AppMetrics
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	_ = godotenv.Load() // Carrega .env para dev local
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := setupTelemetry(ctx)
	if err != nil {
		return fmt.Errorf("inicializar OpenTelemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			log.Printf("Erro ao encerrar OpenTelemetry: %v", err)
		}
	}()

	// --- Configuração ---
	port := os.Getenv("PORT")
	if port == "" {
		port = "8004"
	}

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		return errors.New("REDIS_URL deve ser definida (ex: redis://localhost:6379)")
	}

	flagSvcURL := os.Getenv("FLAG_SERVICE_URL")
	if flagSvcURL == "" {
		return errors.New("FLAG_SERVICE_URL deve ser definida")
	}
	flagSvcURL, err = normalizeServiceBaseURL(flagSvcURL)
	if err != nil {
		return fmt.Errorf("FLAG_SERVICE_URL inválida: %w", err)
	}

	targetingSvcURL := os.Getenv("TARGETING_SERVICE_URL")
	if targetingSvcURL == "" {
		return errors.New("TARGETING_SERVICE_URL deve ser definida")
	}
	targetingSvcURL, err = normalizeServiceBaseURL(targetingSvcURL)
	if err != nil {
		return fmt.Errorf("TARGETING_SERVICE_URL inválida: %w", err)
	}

	// SQS é opcional no dev local, mas obrigatório em prod
	sqsQueueURL := os.Getenv("AWS_SQS_URL")
	sqsEndpoint := os.Getenv("SQS_ENDPOINT")
	awsRegion := os.Getenv("AWS_REGION")
	if sqsQueueURL == "" {
		log.Println("Atenção: AWS_SQS_URL não definida. Eventos não serão enviados.")
	}
	if awsRegion == "" && sqsQueueURL != "" {
		return errors.New("AWS_REGION deve ser definida para usar SQS")
	}

	// --- Inicializa Clientes ---

	// Cliente Redis
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return fmt.Errorf("não foi possível parsear a URL do Redis: %w", err)
	}
	rdb := redis.NewClient(opt)
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Printf("Erro ao fechar Redis: %v", err)
		}
	}()
	if _, err := rdb.Ping(ctx).Result(); err != nil {
		return fmt.Errorf("não foi possível conectar ao Redis: %w", err)
	}
	log.Println("Conectado ao Redis com sucesso!")

	// Cliente SQS (AWS SDK)
	var sqsSvc *sqs.SQS
	if sqsQueueURL != "" {
		awsConfig := &aws.Config{Region: aws.String(awsRegion)}
		if sqsEndpoint != "" {
			awsConfig.Endpoint = aws.String(sqsEndpoint)
		}
		sess, err := session.NewSession(awsConfig)
		if err != nil {
			return fmt.Errorf("não foi possível criar sessão AWS: %w", err)
		}
		sqsSvc = sqs.New(sess)
		log.Println("Cliente SQS inicializado com sucesso.")
	}

	// Cliente HTTP (com timeout)
	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	// Cria a instância da App
	app := &App{
		RedisClient:         rdb,
		SqsSvc:              sqsSvc,
		SqsQueueURL:         sqsQueueURL,
		HttpClient:          httpClient,
		FlagServiceURL:      flagSvcURL,
		TargetingServiceURL: targetingSvcURL,
		Metrics:             newAppMetrics(),
	}

	// --- Rotas ---
	mux := http.NewServeMux()
	mux.HandleFunc("/health", app.healthHandler)
	mux.Handle("/evaluate", otelhttp.NewHandler(http.HandlerFunc(app.evaluationHandler), "GET /evaluate"))

	log.Printf("Serviço de Avaliação (Go) rodando na porta %s", port)
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("servidor HTTP: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("encerrar servidor HTTP: %w", err)
		}
	}

	return nil
}
