package wsapi

// Client is the interface for sending data to WebSocket clients.
type Client interface {
Send(connectionId string, data []byte, dataType string, iamToken string) error
Disconnect(connectionId string, iamToken string) error
}

// NewClient creates a REST or gRPC client based on mode.
func NewClient(mode string) Client {
if mode == "grpc" {
return &grpcClient{}
}
return &restClient{}
}