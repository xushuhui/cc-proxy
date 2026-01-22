package main

// Backend represents an API backend configuration
type Backend struct {
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	Enabled  bool   `json:"enabled"`
	Token    string `json:"token"`
	Model    string `json:"model,omitempty"` // Optional: override model field in request
	Platform string `json:"platform,omitempty"` // Platform type: "anthropic" (default) or "openai"
}

// Config represents configuration file structure
type Config struct {
	Port     int       `json:"port"`
	Backends []Backend `json:"backends"`
	Retry    struct {
		MaxAttempts int `json:"max_attempts"`
		Timeout     int `json:"timeout_seconds"`
	} `json:"retry"`
	Failover struct {
		CircuitBreaker struct {
			FailureThreshold   int `json:"failure_threshold"`
			OpenTimeoutSeconds int `json:"open_timeout_seconds"`
			HalfOpenRequests   int `json:"half_open_requests"`
		} `json:"circuit_breaker"`
		RateLimit struct {
			CooldownSeconds int `json:"cooldown_seconds"`
		} `json:"rate_limit"`
	} `json:"failover"`
}
