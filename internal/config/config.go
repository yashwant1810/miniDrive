package config

import (
	"os"
	"time"
)

type Config struct {
	Port                  string
	DatabaseURL           string
	S3Endpoint            string
	S3Bucket              string
	S3Region              string
	S3AccessKey           string
	S3SecretKey           string
	S3UsePathStyle        bool
	JWTSecret             string
	OrphanCleanupInterval time.Duration
	OrphanMaxAge          time.Duration
}

func Load() *Config {
	return &Config{
		Port:                  getEnv("PORT", "8080"),
		DatabaseURL:           getEnv("DATABASE_URL", "postgres://minidrive:minidrive@localhost:5432/minidrive?sslmode=disable"),
		S3Endpoint:            getEnv("S3_ENDPOINT", "http://localhost:9000"),
		S3Bucket:              getEnv("S3_BUCKET", "minidrive-chunks"),
		S3Region:              getEnv("S3_REGION", "us-east-1"),
		S3AccessKey:           getEnv("S3_ACCESS_KEY", "minioadmin"),
		S3SecretKey:           getEnv("S3_SECRET_KEY", "minioadmin"),
		S3UsePathStyle:        getEnv("S3_USE_PATH_STYLE", "true") == "true",
		JWTSecret:             getEnv("JWT_SECRET", ""),
		OrphanCleanupInterval: parseDuration("ORPHAN_CLEANUP_INTERVAL", 6*time.Hour),
		OrphanMaxAge:          parseDuration("ORPHAN_MAX_AGE", 24*time.Hour),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func parseDuration(key string, fallback time.Duration) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return fallback
	}
	return d
}
