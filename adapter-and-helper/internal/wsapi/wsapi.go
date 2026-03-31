package wsapi

// Client sends data to WebSocket connections via the YC management API.
type Client interface {
	Send(connectionID string, data []byte, dataType string, iamToken string) error
	Disconnect(connectionID string, iamToken string) error
}

// NewClient creates a gRPC client (only gRPC supported in v4).
func NewClient() Client {
	return &grpcClient{}
}
