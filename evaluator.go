package main

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// Tempo de vida do cache em segundos
	CACHE_TTL               = 30 * time.Second
	maxFlagNameLength       = 100
	maxServiceResponseBytes = 1 << 20
)

var errInvalidFlagName = errors.New("nome da flag inválido")

// getDecision é o wrapper principal
func (a *App) getDecision(userID, flagName string) (bool, error) {
	if err := validateFlagName(flagName); err != nil {
		return false, err
	}

	// 1. Obter os dados da flag (do cache ou dos serviços)
	info, err := a.getCombinedFlagInfo(flagName)
	if err != nil {
		return false, err
	}

	// 2. Executar a lógica de avaliação
	return a.runEvaluationLogic(info, userID), nil
}

// getCombinedFlagInfo busca os dados no Redis, com fallback para os microsserviços
func (a *App) getCombinedFlagInfo(flagName string) (*CombinedFlagInfo, error) {
	cacheKey := fmt.Sprintf("flag_info:%s", flagName)

	// 1. Tentar buscar do Cache (Redis)
	val, err := a.RedisClient.Get(ctx, cacheKey).Result()
	if err == nil {
		// Cache HIT
		var info CombinedFlagInfo
		if unmarshalErr := json.Unmarshal([]byte(val), &info); unmarshalErr == nil {
			log.Printf("Cache HIT para flag '%s'", flagName)
			return &info, nil
		} else {
			// Se o unmarshal falhar, trata como cache miss
			log.Printf("Erro ao desserializar cache para flag '%s': %v", flagName, unmarshalErr)
		}
	}

	log.Printf("Cache MISS para flag '%s'", flagName)
	// 2. Cache MISS - Buscar dos serviços
	info, err := a.fetchFromServices(flagName)
	if err != nil {
		return nil, err
	}

	// 3. Salvar no Cache
	jsonData, err := json.Marshal(info)
	if err == nil {
		if err := a.RedisClient.Set(ctx, cacheKey, jsonData, CACHE_TTL).Err(); err != nil {
			log.Printf("Erro ao salvar flag '%s' no cache: %v", flagName, err)
		}
	}

	return info, nil
}

// fetchFromServices busca dados do flag-service e targeting-service concorrentemente
func (a *App) fetchFromServices(flagName string) (*CombinedFlagInfo, error) {
	var wg sync.WaitGroup
	wg.Add(2)

	var flagInfo *Flag
	var ruleInfo *TargetingRule
	var flagErr, ruleErr error

	// Goroutine 1: Buscar do flag-service
	go func() {
		defer wg.Done()
		flagInfo, flagErr = a.fetchFlag(flagName)
	}()

	// Goroutine 2: Buscar do targeting-service
	go func() {
		defer wg.Done()
		ruleInfo, ruleErr = a.fetchRule(flagName)
	}()

	wg.Wait()

	if flagErr != nil {
		return nil, flagErr
	}
	if ruleErr != nil {
		log.Printf("Aviso: Nenhuma regra de segmentação encontrada para '%s'. Usando padrão.", flagName)
	}

	return &CombinedFlagInfo{
		Flag: flagInfo,
		Rule: ruleInfo,
	}, nil
}

func validateFlagName(flagName string) error {
	if flagName == "" || !utf8.ValidString(flagName) || utf8.RuneCountInString(flagName) > maxFlagNameLength {
		return errInvalidFlagName
	}
	if flagName == "." || flagName == ".." || strings.ContainsAny(flagName, `/\`) {
		return errInvalidFlagName
	}
	for _, character := range flagName {
		if unicode.IsControl(character) {
			return errInvalidFlagName
		}
	}
	return nil
}

func normalizeServiceBaseURL(rawURL string) (string, error) {
	baseURL, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("URL de serviço inválida: %w", err)
	}
	if (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Hostname() == "" {
		return "", errors.New("URL de serviço deve usar http ou https e possuir host")
	}
	if baseURL.User != nil || baseURL.Opaque != "" || baseURL.RawQuery != "" || baseURL.ForceQuery || baseURL.Fragment != "" {
		return "", errors.New("URL de serviço não pode conter credenciais, query ou fragmento")
	}

	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	baseURL.RawPath = ""
	return baseURL.String(), nil
}

func newServiceRequest(baseURL, resource, flagName string) (*http.Request, error) {
	if err := validateFlagName(flagName); err != nil {
		return nil, err
	}

	normalizedBaseURL, err := normalizeServiceBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(normalizedBaseURL)
	if err != nil {
		return nil, fmt.Errorf("erro ao processar URL do serviço: %w", err)
	}
	endpoint.Path += "/" + resource + "/" + flagName

	// #nosec G704 -- o esquema e o host vêm da URL-base validada; a entrada do cliente é apenas um segmento de caminho validado.
	request, err := http.NewRequest(http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("erro ao criar requisição para o serviço: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+os.Getenv("SERVICE_API_KEY"))
	return request, nil
}

func (a *App) doServiceRequest(request *http.Request) (*http.Response, error) {
	if a.HttpClient == nil {
		return nil, errors.New("cliente HTTP não configurado")
	}

	// Não seguir redirects impede que um serviço permitido redirecione a chamada
	// (e o cabeçalho de autorização) para outro host.
	client := *a.HttpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// #nosec G704 -- a requisição é criada por newServiceRequest com origem e caminho validados, e redirects estão desabilitados.
	return client.Do(request)
}

func closeResponseBody(body io.Closer) {
	if err := body.Close(); err != nil {
		log.Printf("Erro ao fechar resposta HTTP: %v", err)
	}
}

// fetchFlag (função helper)
func (a *App) fetchFlag(flagName string) (*Flag, error) {
	req, err := newServiceRequest(a.FlagServiceURL, "flags", flagName)
	if err != nil {
		return nil, fmt.Errorf("erro ao preparar chamada ao flag-service: %w", err)
	}

	resp, err := a.doServiceRequest(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao chamar flag-service: %w", err)
	}
	defer closeResponseBody(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{flagName}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("flag-service retornou status %d", resp.StatusCode)
	}

	var flag Flag
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxServiceResponseBytes)).Decode(&flag); err != nil {
		return nil, fmt.Errorf("erro ao desserializar resposta do flag-service: %w", err)
	}
	return &flag, nil
}

func (a *App) fetchRule(flagName string) (*TargetingRule, error) {
	req, err := newServiceRequest(a.TargetingServiceURL, "rules", flagName)
	if err != nil {
		return nil, fmt.Errorf("erro ao preparar chamada ao targeting-service: %w", err)
	}

	resp, err := a.doServiceRequest(req)
	if err != nil {
		return nil, fmt.Errorf("erro ao chamar targeting-service: %w", err)
	}
	defer closeResponseBody(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{flagName} // Não é um erro fatal
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("targeting-service retornou status %d", resp.StatusCode)
	}

	var rule TargetingRule
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxServiceResponseBytes)).Decode(&rule); err != nil {
		return nil, fmt.Errorf("erro ao desserializar resposta do targeting-service: %w", err)
	}
	return &rule, nil
}

// runEvaluationLogic é onde a decisão é tomada
func (a *App) runEvaluationLogic(info *CombinedFlagInfo, userID string) bool {
	if info.Flag == nil || !info.Flag.IsEnabled {
		return false
	}

	if info.Rule == nil || !info.Rule.IsEnabled {
		return true
	}

	// 3. Processa a regra (só temos "PERCENTAGE" por enquanto)
	rule := info.Rule.Rules
	if rule.Type == "PERCENTAGE" {
		// Converte o 'value' (que é interface{}) para float64
		percentage, ok := rule.Value.(float64)
		if !ok {
			log.Printf("Erro: valor da regra de porcentagem não é um número para a flag '%s'", info.Flag.Name)
			return false
		}

		// Calcula o "bucket" do usuário (0-99)
		userBucket := getDeterministicBucket(userID + info.Flag.Name)

		if float64(userBucket) < percentage {
			return true
		}
	}

	return false
}

func getDeterministicBucket(input string) int {
	// Usamos SHA1 (rápido) e pegamos os primeiros 4 bytes
	hasher := sha1.New()
	hasher.Write([]byte(input))
	hash := hasher.Sum(nil)

	// Converte 4 bytes para um uint32
	val := binary.BigEndian.Uint32(hash[:4])

	// Retorna o módulo 100
	return int(val % 100)
}
