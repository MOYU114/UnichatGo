package worker

import (
	"log"
	"os"
	"strings"
)

var workerDebugEnabled = strings.EqualFold(os.Getenv("UNICHATGO_WORKER_DEBUG"), "1")

func debugLog(format string, args ...interface{}) {
	if workerDebugEnabled {
		log.Printf(format, args...)
	}
}
