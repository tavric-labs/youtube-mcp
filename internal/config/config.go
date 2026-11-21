// Package config reads the YouTube MCP server's settings from the environment.
package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config is everything the server needs to start.
type Config struct {
	Port, MCPPath string
	// Resource is the canonical public MCP URL (the audience of mcp-auth tokens).
	Resource string
	// Issuer is the mcp-auth issuer URL. JWKSURL is discovered from it unless set.
	Issuer, JWKSURL string
	TokenEndpoint   string
	ClientID, KeyID string
	PrivateKey      *rsa.PrivateKey
	// APIBase and UploadBase are the YouTube Data API v3 endpoints. They are
	// overridable so tests and proxies can stand in for Google.
	APIBase, UploadBase string
	// MediaDir is where uploads are staged, one private directory per person.
	MediaDir string
	// AllowInsecure permits http issuer and token endpoints, for local development.
	AllowInsecure bool
}

// Load reads and validates the configuration. getenv is os.Getenv in production.
func Load(getenv func(string) string) (Config, error) {
	value := func(name, fallback string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	config := Config{
		Port:          value("PORT", "8080"),
		MCPPath:       value("MCP_PATH", "/mcp"),
		Resource:      value("YOUTUBE_MCP_RESOURCE", ""),
		Issuer:        strings.TrimRight(value("YOUTUBE_MCP_ISSUER", ""), "/"),
		JWKSURL:       value("YOUTUBE_JWKS_URL", ""),
		ClientID:      value("YOUTUBE_MCP_CLIENT_ID", ""),
		KeyID:         value("YOUTUBE_MCP_CLIENT_KEY_ID", ""),
		APIBase:       strings.TrimRight(value("YOUTUBE_API_BASE", "https://www.googleapis.com/youtube/v3"), "/"),
		UploadBase:    strings.TrimRight(value("YOUTUBE_UPLOAD_BASE", "https://www.googleapis.com/upload/youtube/v3"), "/"),
		MediaDir:      value("YOUTUBE_MCP_MEDIA_DIR", "/var/lib/youtube-mcp/media"),
		AllowInsecure: value("YOUTUBE_MCP_ALLOW_INSECURE", "") == "true",
	}
	for name, v := range map[string]string{
		"YOUTUBE_MCP_RESOURCE": config.Resource, "YOUTUBE_MCP_ISSUER": config.Issuer,
		"YOUTUBE_MCP_CLIENT_ID": config.ClientID, "YOUTUBE_MCP_CLIENT_KEY_ID": config.KeyID,
	} {
		if strings.TrimSpace(v) == "" {
			return Config{}, fmt.Errorf("%s is required", name)
		}
	}
	if err := absoluteURL("YOUTUBE_MCP_RESOURCE", config.Resource, config.AllowInsecure); err != nil {
		return Config{}, err
	}
	if err := absoluteURL("YOUTUBE_MCP_ISSUER", config.Issuer, config.AllowInsecure); err != nil {
		return Config{}, err
	}
	if err := absoluteURL("YOUTUBE_API_BASE", config.APIBase, false); err != nil {
		return Config{}, err
	}
	if err := absoluteURL("YOUTUBE_UPLOAD_BASE", config.UploadBase, false); err != nil {
		return Config{}, err
	}
	if !filepath.IsAbs(config.MediaDir) {
		return Config{}, errors.New("YOUTUBE_MCP_MEDIA_DIR must be an absolute path")
	}
	config.TokenEndpoint = config.Issuer + "/token"
	key, err := clientKey(getenv("YOUTUBE_MCP_CLIENT_KEY"), getenv("YOUTUBE_MCP_CLIENT_KEY_FILE"))
	if err != nil {
		return Config{}, err
	}
	config.PrivateKey = key
	return config, nil
}

func clientKey(inline, path string) (*rsa.PrivateKey, error) {
	inline, path = strings.TrimSpace(inline), strings.TrimSpace(path)
	switch {
	case inline != "" && path != "":
		return nil, errors.New("set YOUTUBE_MCP_CLIENT_KEY or YOUTUBE_MCP_CLIENT_KEY_FILE, not both")
	case inline != "":
		return parseKey([]byte(inline))
	case path != "":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read the client key file: %w", err)
		}
		return parseKey(data)
	}
	return nil, errors.New("YOUTUBE_MCP_CLIENT_KEY or YOUTUBE_MCP_CLIENT_KEY_FILE is required")
}

func absoluteURL(name, raw string, allowHTTP bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http")) {
		return fmt.Errorf("%s must be an absolute https URL", name)
	}
	return nil
}

func parseKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("the client key is not PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := parsed.(*rsa.PrivateKey); ok {
			return key, nil
		}
		return nil, errors.New("the client key must be an RSA key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the client key is not a PKCS#8 or PKCS#1 RSA key")
	}
	return key, nil
}
