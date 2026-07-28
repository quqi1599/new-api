package common

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func GetEnvOrDefault(env string, defaultValue int) int {
	if env == "" || os.Getenv(env) == "" {
		return defaultValue
	}
	num, err := strconv.Atoi(os.Getenv(env))
	if err != nil {
		SysError(fmt.Sprintf("failed to parse %s: %s, using default value: %d", env, err.Error(), defaultValue))
		return defaultValue
	}
	return num
}

func GetEnvOrDefaultString(env string, defaultValue string) string {
	if env == "" || os.Getenv(env) == "" {
		return defaultValue
	}
	return os.Getenv(env)
}

func GetEnvOrDefaultBool(env string, defaultValue bool) bool {
	if env == "" || os.Getenv(env) == "" {
		return defaultValue
	}
	b, err := strconv.ParseBool(os.Getenv(env))
	if err != nil {
		SysError(fmt.Sprintf("failed to parse %s: %s, using default value: %t", env, err.Error(), defaultValue))
		return defaultValue
	}
	return b
}

func ParseTokenRPMRateLimits(raw string) (map[int]int, error) {
	limits := make(map[int]int)
	if strings.TrimSpace(raw) == "" {
		return limits, nil
	}

	for _, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid token RPM limit %q, expected token_id:rpm", entry)
		}

		tokenID, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 32)
		if err != nil || tokenID <= 0 {
			return nil, fmt.Errorf("invalid token id in token RPM limit %q", entry)
		}
		rpm, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 32)
		if err != nil || rpm <= 0 {
			return nil, fmt.Errorf("invalid RPM in token RPM limit %q", entry)
		}
		if _, exists := limits[int(tokenID)]; exists {
			return nil, fmt.Errorf("duplicate token id %d in token RPM limits", tokenID)
		}
		limits[int(tokenID)] = int(rpm)
	}

	return limits, nil
}
