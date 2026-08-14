package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var (
	ConfigPath string
	KeyPath    string
	keyCache   []byte
)

func initPaths() {
	execPath, err := os.Executable()
	if err == nil {
		baseDir := filepath.Dir(execPath)
		ConfigPath = filepath.Join(baseDir, "config.json")
		KeyPath = filepath.Join(baseDir, "config.key")

		cwd, _ := os.Getwd()
		if _, err := os.Stat(filepath.Join(cwd, "config.json")); err == nil {
			ConfigPath = filepath.Join(cwd, "config.json")
			KeyPath = filepath.Join(cwd, "config.key")
		}
	} else {
		cwd, _ := os.Getwd()
		ConfigPath = filepath.Join(cwd, "config.json")
		KeyPath = filepath.Join(cwd, "config.key")
	}
}

func init() {
	initPaths()
}

// Config represents the config.json file
type Config map[string]interface{}

// LoadConfig loads the configuration from ConfigPath
func LoadConfig() Config {
	if _, err := os.Stat(ConfigPath); err != nil {
		return make(Config)
	}
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		fmt.Printf("Error reading config: %v\n", err)
		return make(Config)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Printf("Error unmarshaling config: %v\n", err)
		return make(Config)
	}
	return cfg
}

// OrderedConfig defines the order of fields in config.json
// Matches Python config.example.json standard exactly
type OrderedConfig struct {
	Username     string                 `json:"username"`
	Password     string                 `json:"password,omitempty"`
	Port         int                    `json:"port"`
	DownloadPath string                 `json:"download_path"`
	WosSid       string                 `json:"wos_sid"`
	WosCookies   map[string]interface{} `json:"wos_cookies"`
	CnkiCookies  map[string]interface{} `json:"cnki_cookies"`
}

func getStr(cfg Config, key string) string {
	if val, ok := cfg[key].(string); ok {
		return val
	}
	return ""
}

func getInt(cfg Config, key string, defaultVal int) int {
	if val, ok := cfg[key].(float64); ok {
		return int(val)
	}
	if val, ok := cfg[key].(int); ok {
		return val
	}
	return defaultVal
}

func getMap(cfg Config, key string) map[string]interface{} {
	if val, ok := cfg[key].(map[string]interface{}); ok {
		return val
	}
	return make(map[string]interface{})
}

// SaveConfig saves the configuration to ConfigPath
func SaveConfig(cfg Config) error {

	pwd := getStr(cfg, "password")
	if pwd == "" {
		if enc := getStr(cfg, "password_enc"); enc != "" {
			if dec, err := DecryptSecret(enc); err == nil {
				pwd = dec
			}
		}
	}

	oc := OrderedConfig{
		Username:     getStr(cfg, "username"),
		Password:     pwd,
		Port:         getInt(cfg, "port", 5000),
		DownloadPath: getStr(cfg, "download_path"),
		WosSid:       getStr(cfg, "wos_sid"),
		WosCookies:   getMap(cfg, "wos_cookies"),
		CnkiCookies:  getMap(cfg, "cnki_cookies"),
	}
	if oc.DownloadPath == "" {
		oc.DownloadPath = "download"
	}

	data, err := json.MarshalIndent(oc, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath, data, 0644)
}

func getKey() []byte {
	if keyCache != nil {
		return keyCache
	}
	envKey := os.Getenv("WOS_CONFIG_KEY")
	if envKey != "" {
		hash := sha256.Sum256([]byte(envKey))
		keyCache = hash[:]
		return keyCache
	}

	if data, err := os.ReadFile(KeyPath); err == nil && len(data) == 32 {
		keyCache = data
		return keyCache
	}

	keyCache = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, keyCache); err != nil {
		panic("Failed to generate random key")
	}
	if err := os.WriteFile(KeyPath, keyCache, 0600); err != nil {
		fmt.Printf("[config_util] Failed to write key file: %v\n", err)
	} else {
		fmt.Printf("[config_util] Generated config key file: %s (Do not share/delete)\n", KeyPath)
	}
	return keyCache
}

// EncryptSecret encrypts a plaintext string using AES-256-GCM.
func EncryptSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	key := getKey()
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ciphertext := aesgcm.Seal(nil, nonce, []byte(plain), nil)

	finalData := append(nonce, ciphertext...)
	return base64.StdEncoding.EncodeToString(finalData), nil
}

// DecryptSecret decrypts a token string encrypted by EncryptSecret.
func DecryptSecret(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	key := getKey()
	data, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}
	if len(data) < 28 {
		return "", errors.New("invalid encrypted data length")
	}
	nonce, ciphertext := data[:12], data[12:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// GetCredentials retrieves username and password from env or config.
func GetCredentials(cfg Config) (string, string) {
	username := os.Getenv("WOS_USERNAME")
	if username == "" {
		if u, ok := cfg["username"].(string); ok {
			username = u
		}
	}

	password := os.Getenv("WOS_PASSWORD")
	if password == "" {
		for _, encField := range []string{"password_enc", "wos_password_enc", "cnki_password_enc"} {
			if val, ok := cfg[encField].(string); ok && val != "" {
				dec, err := DecryptSecret(val)
				if err == nil && dec != "" {
					password = dec
					break
				}
			}
		}
		if password == "" {
			for _, field := range []string{"password", "wos_password", "cnki_password"} {
				if val, ok := cfg[field].(string); ok && val != "" {
					password = val
					break
				}
			}
		}
	}
	return username, password
}

// SetCredentials sets username and password in config (plain text, matching Python standard).
func SetCredentials(cfg Config, username, password string) {
	cfg["username"] = username
	cfg["password"] = password

	for _, field := range []string{"password_enc", "wos_password", "cnki_password", "wos_password_enc", "cnki_password_enc"} {
		delete(cfg, field)
	}
}
