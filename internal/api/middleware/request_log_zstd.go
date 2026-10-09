package middleware

import (
	"io"

	"github.com/klauspost/compress/zstd"
)

const (
	// maxDecodedRequestBodyLogBytes caps how much of a compressed request body is
	// decompressed for a log entry. Request bodies are captured before
	// authentication, so a small compressed body must not expand without bound.
	maxDecodedRequestBodyLogBytes = maxDeferredErrorRequestBodyBytes
	// maxRequestLogZstdWindowBytes rejects frames whose header asks for a larger
	// decoding window, so a crafted header cannot force a huge allocation.
	// 8 MiB is the window every zstd level up to 19 stays within.
	maxRequestLogZstdWindowBytes = 8 << 20
)

// newRequestLogZstdDecoder returns a zstd decoder with bounded memory use for
// decoding request bodies into log entries.
func newRequestLogZstdDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(maxRequestLogZstdWindowBytes),
		zstd.WithDecoderMaxMemory(uint64(maxDecodedRequestBodyLogBytes)),
	)
}
