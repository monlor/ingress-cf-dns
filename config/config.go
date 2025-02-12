package config

import (
    "regexp"
    "time"

    "github.com/caarlos0/env/v10"
)

// Config holds all configuration for the controller
type Config struct {
    // Cloudflare configuration
    CloudflareToken string `env:"CF_TOKEN,required" envDescription:"Cloudflare API token"`

    // Kubernetes configuration
    KubeconfigPath string `env:"KUBECONFIG" envDefault:"" envDescription:"Path to kubeconfig file (optional)"`

    // Controller configuration
    SyncInterval time.Duration `env:"SYNC_INTERVAL_SECONDS" envDefault:"300s" envDescription:"Interval between DNS sync operations"`
    NodePublicIPAnnotation string `env:"NODE_PUBLIC_IP_ANNOTATION" envDefault:"node.kubernetes.io/public-ip" envDescription:"Node annotation key for public IP"`
    DNSProxied bool `env:"DNS_PROXIED" envDefault:"true" envDescription:"Whether to use Cloudflare's proxy service"`

    // Filter configuration
    NamespaceRegex string `env:"NAMESPACE_REGEX" envDefault:".*" envDescription:"Regex pattern to filter namespaces"`
    DomainRegex string `env:"DOMAIN_REGEX" envDefault:".*" envDescription:"Regex pattern to filter domain names"`

    // Compiled regex patterns (not from env)
    namespacePattern *regexp.Regexp
    domainPattern *regexp.Regexp
}

const (
    DNSRecordComment = "managed by Ingress-CF-DNS"
)

// LoadConfig loads configuration from environment variables
func LoadConfig() (*Config, error) {
    cfg := &Config{}
    if err := env.Parse(cfg); err != nil {
        return nil, err
    }

    // Compile regex patterns
    var err error
    cfg.namespacePattern, err = regexp.Compile(cfg.NamespaceRegex)
    if err != nil {
        return nil, err
    }

    cfg.domainPattern, err = regexp.Compile(cfg.DomainRegex)
    if err != nil {
        return nil, err
    }

    return cfg, nil
}

// IsNamespaceAllowed checks if a namespace matches the configured pattern
func (c *Config) IsNamespaceAllowed(namespace string) bool {
    return c.namespacePattern.MatchString(namespace)
}

// IsDomainAllowed checks if a domain matches the configured pattern
func (c *Config) IsDomainAllowed(domain string) bool {
    return c.domainPattern.MatchString(domain)
} 