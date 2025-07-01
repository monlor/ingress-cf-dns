package config

import (
	"strings"
	"time"

	"github.com/caarlos0/env/v10"
	regexp "github.com/dlclark/regexp2"
)

// Config holds all configuration for the controller
type Config struct {
	// Cloudflare configuration
	CloudflareToken string `env:"CF_TOKEN,required" envDescription:"Cloudflare API token"`

	// Kubernetes configuration
	KubeconfigPath string `env:"KUBECONFIG" envDefault:"" envDescription:"Path to kubeconfig file (optional)"`

	// Controller configuration
	SyncInterval           time.Duration `env:"SYNC_INTERVAL_SECONDS" envDefault:"300s" envDescription:"Interval between DNS sync operations"`
	NodePublicIPAnnotation string        `env:"NODE_PUBLIC_IP_ANNOTATION" envDefault:"node.kubernetes.io/public-ip" envDescription:"Node annotation key for public IP"`
	DNSProxied             bool          `env:"DNS_PROXIED" envDefault:"true" envDescription:"Whether to use Cloudflare's proxy service"`

	// Filter configuration
	NamespaceRegex string `env:"NAMESPACE_REGEX" envDefault:".*" envDescription:"Regex pattern to filter namespaces"`
	DomainRegex    string `env:"DOMAIN_REGEX" envDefault:".*" envDescription:"Regex pattern to filter domain names"`
	
	// Ingress label filter configuration
	IngressLabelKey   string `env:"INGRESS_LABEL_KEY" envDefault:"ingress-cf-dns.k8s.io/enabled" envDescription:"Label key to enable DNS management for ingress"`
	IngressLabelValue string `env:"INGRESS_LABEL_VALUE" envDefault:"true" envDescription:"Label value to enable DNS management for ingress"`

	// Ingress annotation configuration
	IngressProxiedAnnotation string `env:"INGRESS_PROXIED_ANNOTATION" envDefault:"ingress-cf-dns.k8s.io/proxied" envDescription:"Annotation key to control DNS record proxied status"`

	// Compiled regex patterns (not from env)
	namespacePattern *regexp.Regexp
	domainPattern    *regexp.Regexp
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
	cfg.namespacePattern, err = regexp.Compile(cfg.NamespaceRegex, regexp.None)
	if err != nil {
		return nil, err
	}

	cfg.domainPattern, err = regexp.Compile(cfg.DomainRegex, regexp.None)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// IsNamespaceAllowed checks if a namespace matches the configured pattern
func (c *Config) IsNamespaceAllowed(namespace string) bool {
	match, _ := c.namespacePattern.MatchString(namespace)
	return match
}

// IsDomainAllowed checks if a domain matches the configured pattern
func (c *Config) IsDomainAllowed(domain string) bool {
	match, _ := c.domainPattern.MatchString(domain)
	return match
}

// IsIngressDNSEnabled checks if an ingress has the required label to enable DNS management
func (c *Config) IsIngressDNSEnabled(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	
	value, exists := labels[c.IngressLabelKey]
	return exists && value == c.IngressLabelValue
}

// GetIngressProxiedValue gets the proxied value from ingress annotations, defaults to true
func (c *Config) GetIngressProxiedValue(annotations map[string]string) bool {
	if annotations == nil {
		return true // 默认值为true
	}
	
	value, exists := annotations[c.IngressProxiedAnnotation]
	if !exists {
		return true // 默认值为true
	}
	
	// 解析字符串值为布尔值
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		// 如果值无法解析，返回默认值true
		return true
	}
}
