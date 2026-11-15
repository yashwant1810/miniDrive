package model

type Chunk struct {
	Hash     string `json:"hash"`
	Size     int    `json:"size"`
	S3Key    string `json:"s3_key"`
	RefCount int    `json:"ref_count"`
}

type VersionChunk struct {
	VersionID  string `json:"version_id"`
	ChunkIndex int    `json:"chunk_index"`
	ChunkHash  string `json:"chunk_hash"`
}
