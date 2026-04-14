package config

import (
	"os"
	"runtime"
	"strconv"
)

var (
	MaxImpactNodes  = envInt("CRG_MAX_IMPACT_NODES", 500)
	MaxImpactDepth  = envInt("CRG_MAX_IMPACT_DEPTH", 2)
	MaxBFSDepth     = envInt("CRG_MAX_BFS_DEPTH", 15)
	MaxSearchResults = envInt("CRG_MAX_SEARCH_RESULTS", 20)
	ParseWorkers    = envInt("CRG_PARSE_WORKERS", min(runtime.NumCPU(), 8))
	GitTimeout      = envInt("CRG_GIT_TIMEOUT", 30)
	DependentHops   = envInt("CRG_DEPENDENT_HOPS", 2)
	MaxDependentFiles = 500
)

var SecurityKeywords = map[string]struct{}{
	"auth": {}, "login": {}, "password": {}, "token": {},
	"session": {}, "crypt": {}, "secret": {}, "credential": {},
	"permission": {}, "sql": {}, "query": {}, "execute": {},
	"connect": {}, "socket": {}, "request": {}, "http": {},
	"sanitize": {}, "validate": {}, "encrypt": {}, "decrypt": {},
	"hash": {}, "sign": {}, "verify": {}, "admin": {}, "privilege": {},
}

var DefaultIgnorePatterns = []string{
	".code-review-graph/**",
	"node_modules/**",
	".git/**",
	"__pycache__/**",
	"*.pyc",
	".venv/**",
	"venv/**",
	"dist/**",
	"build/**",
	".next/**",
	"target/**",
	"vendor/**",
	"bootstrap/cache/**",
	"public/build/**",
	".bundle/**",
	".gradle/**",
	"*.jar",
	".dart_tool/**",
	".pub-cache/**",
	"coverage/**",
	".cache/**",
	"*.min.js",
	"*.min.css",
	"*.map",
	"*.lock",
	"package-lock.json",
	"yarn.lock",
	"*.db",
	"*.sqlite",
	"*.db-journal",
	"*.db-wal",
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func SerialParse() bool {
	return os.Getenv("CRG_SERIAL_PARSE") == "1"
}

func RepoRootOverride() string {
	return os.Getenv("CRG_REPO_ROOT")
}

func DataDirOverride() string {
	return os.Getenv("CRG_DATA_DIR")
}
