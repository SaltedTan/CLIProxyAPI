package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func compressZstdForTest(t *testing.T, payload []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	var compressed bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&compressed, opts...)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter: %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}
	return compressed.Bytes()
}

// newAuthGatedLoggingRouter mirrors the server's ordering: request logging runs
// before authentication, which rejects requests that carry no API key.
func newAuthGatedLoggingRouter(logger logging.RequestLogger, handler gin.HandlerFunc) *gin.Engine {
	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.Use(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer test-key" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing api key"})
			return
		}
		c.Next()
	})
	router.POST("/v1/responses", handler)
	return router
}

func readSingleLogFile(t *testing.T, dir string) []byte {
	t.Helper()
	matches, errGlob := filepath.Glob(filepath.Join(dir, "*.log"))
	if errGlob != nil {
		t.Fatalf("glob logs: %v", errGlob)
	}
	if len(matches) != 1 {
		t.Fatalf("log files = %v, want exactly one", matches)
	}
	content, errRead := os.ReadFile(matches[0])
	if errRead != nil {
		t.Fatalf("read log: %v", errRead)
	}
	return content
}

func TestRequestLoggingMiddlewareBoundsAnonymousZstdExpansion(t *testing.T) {
	gin.SetMode(gin.TestMode)

	bomb := compressZstdForTest(t, make([]byte, maxDecodedRequestBodyLogBytes+8<<20))
	if int64(len(bomb)) > maxErrorOnlyCapturedRequestBodyBytes {
		t.Fatalf("compressed bomb is %d bytes, want it small enough for eager capture", len(bomb))
	}

	for _, enabled := range []bool{true, false} {
		name := "request log disabled"
		if enabled {
			name = "request log enabled"
		}
		t.Run(name, func(t *testing.T) {
			logsDir := t.TempDir()
			logger := logging.NewFileRequestLogger(enabled, logsDir, "", 10)
			handlerCalled := false
			router := newAuthGatedLoggingRouter(logger, func(c *gin.Context) {
				handlerCalled = true
				c.Status(http.StatusOK)
			})

			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(bomb))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Content-Encoding", "zstd")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Fatalf("response status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
			if handlerCalled {
				t.Fatal("handler ran for an unauthenticated request")
			}
			content := readSingleLogFile(t, logsDir)
			// Without request logging only an error log is written, and its decoded
			// body must stay within the raw-size cap for error-only capture.
			limit := maxDecodedRequestBodyLogBytes + 64<<10
			if !enabled {
				limit = maxErrorOnlyCapturedRequestBodyBytes + 64<<10
			}
			if int64(len(content)) > limit {
				t.Fatalf("log entry is %d bytes, want at most %d", len(content), limit)
			}
			if !bytes.Contains(content, []byte("[DECOMPRESSED REQUEST BODY TRUNCATED]")) {
				t.Fatal("log entry does not mark the decompressed request body as truncated")
			}
		})
	}
}

func TestRequestLoggingMiddlewareLogsDecodedZstdBodyAndForwardsRawBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	payload := []byte(`{"model":"test-model","input":"` + strings.Repeat("hello ", 512) + `"}`)
	compressed := compressZstdForTest(t, payload)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 10)
	var forwarded []byte
	router := newAuthGatedLoggingRouter(logger, func(c *gin.Context) {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		forwarded = body
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressed))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusOK)
	}
	if !bytes.Equal(forwarded, compressed) {
		t.Fatal("handler did not receive the original compressed request bytes")
	}
	content := readSingleLogFile(t, logsDir)
	if !bytes.Contains(content, payload) {
		t.Fatal("log entry does not contain the decoded request body")
	}
	if bytes.Contains(content, []byte("TRUNCATED")) {
		t.Fatal("log entry marks a small request body as truncated")
	}
}

func TestDecodeCapturedRequestBodyForLogRejectsOversizedZstdWindow(t *testing.T) {
	payload := []byte(`{"model":"test-model"}`)
	// A hand-built frame whose header asks for a 64 MiB window (window log 26)
	// and carries the payload in one raw block.
	compressed := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x80}
	blockHeader := uint32(1) | uint32(len(payload))<<3
	compressed = append(compressed, byte(blockHeader), byte(blockHeader>>8), byte(blockHeader>>16))
	compressed = append(compressed, payload...)

	var header zstd.Header
	if errHeader := header.Decode(compressed); errHeader != nil {
		t.Fatalf("decode frame header: %v", errHeader)
	}
	if header.WindowSize <= maxRequestLogZstdWindowBytes {
		t.Fatalf("test frame window = %d, want more than %d", header.WindowSize, maxRequestLogZstdWindowBytes)
	}

	decoded := decodeCapturedRequestBodyForLogWithLimit(compressed, "zstd", maxDecodedRequestBodyLogBytes)
	if !bytes.Equal(decoded, compressed) {
		t.Fatalf("decoded = %q, want the raw bytes kept for a frame with an oversized window", decoded)
	}
}
