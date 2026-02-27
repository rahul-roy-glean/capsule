package snapshot

import "context"

// ChunkStorer abstracts chunk storage for testing.
// *ChunkStore satisfies this interface.
type ChunkStorer interface {
	StoreChunk(ctx context.Context, data []byte) (hash string, compressedSize int64, err error)
	GetChunk(ctx context.Context, hash string) ([]byte, error)
}
