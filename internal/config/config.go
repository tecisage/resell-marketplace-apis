package config

import (
	"os"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port               string
	Env                string
	DB_URL             string
	JWTSecret          string
	R2_ACCOUNT_ID      string
	R2_ACCESS_KEY      string
	R2_ACCESS_SECRET   string
	R2_BUCKET          string
	AllowedOrigins     []string
	TrustedProxyHeader string
}

func MustLoad() Config {
	godotenv.Load()

	port := os.Getenv("PORT")
	if port == "" {
		panic("PORT is not set")
	}

	env := os.Getenv("ENV")
	if env == "" {
		panic("ENV is not set")
	}

	db_url := os.Getenv("DB_URL")
	if db_url == "" {
		panic("DB_URL is not set")
	}

	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		panic("JWT_SECRET is not set")
	}

	trustedProxyHeader := os.Getenv("TRUSTED_PROXY_HEADER")

	R2_ACCOUNT_ID := os.Getenv("R2_ACCOUNT_ID")
	if R2_ACCOUNT_ID == "" {
		panic("R2_ACCOUNT_ID is not set")
	}

	R2_ACCESS_KEY := os.Getenv("R2_ACCESS_KEY")
	if R2_ACCESS_KEY == "" {
		panic("R2_ACCESS_KEY is not set")
	}

	R2_ACCESS_SECRET := os.Getenv("R2_ACCESS_SECRET")
	if R2_ACCESS_SECRET == "" {
		panic("R2_ACCESS_SECRET is not set")
	}

	R2_BUCKET := os.Getenv("R2_BUCKET")
	if R2_BUCKET == "" {
		panic("R2_BUCKET is not set")
	}

	rawAllowedOrigins := os.Getenv("ALLOWED_ORIGINS")
	var allowedOrigins []string
	if rawAllowedOrigins != "" {
		for _, o := range strings.Split(rawAllowedOrigins, ",") {
			trimmed := strings.TrimRight(strings.TrimSpace(o), "/")
			if trimmed != "" {
				allowedOrigins = append(allowedOrigins, trimmed)
			}
		}
	} else {
		allowedOrigins = []string{"http://localhost:5173"}
	}

	return Config{
		Port:               port,
		Env:                env,
		DB_URL:             db_url,
		JWTSecret:          jwtSecret,
		R2_ACCOUNT_ID:      R2_ACCOUNT_ID,
		R2_ACCESS_KEY:      R2_ACCESS_KEY,
		R2_ACCESS_SECRET:   R2_ACCESS_SECRET,
		R2_BUCKET:          R2_BUCKET,
		AllowedOrigins:     allowedOrigins,
		TrustedProxyHeader: trustedProxyHeader,
	}
}
