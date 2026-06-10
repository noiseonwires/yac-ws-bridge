package wsapi

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	ws "github.com/yandex-cloud/go-genproto/yandex/cloud/serverless/apigateway/websocket/v1"
)

const grpcEndpoint = "apigateway-connections.api.cloud.yandex.net:443"

// callTimeout bounds each wsSend/Disconnect RPC. Without it a hung YC API call
// would block the calling stream's read loop indefinitely (no progress, no
// error, no teardown). The data path is normally sub-second; this is only a
// safety net so a stuck call eventually fails and the stream can recover.
const callTimeout = 20 * time.Second

type grpcClient struct {
	mu     sync.Mutex
	client ws.ConnectionServiceClient
	conn   *grpc.ClientConn
}

// ensure lazily builds the gRPC client. Unlike a sync.Once, a transient
// failure is NOT cached permanently: the next call retries, so the process can
// recover instead of being wedged for its whole lifetime by one early error.
func (g *grpcClient) ensure() (ws.ConnectionServiceClient, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.client != nil {
		return g.client, nil
	}
	creds := credentials.NewTLS(&tls.Config{})
	conn, err := grpc.NewClient(grpcEndpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("grpc dial: %w", err)
	}
	g.conn = conn
	g.client = ws.NewConnectionServiceClient(conn)
	log.Println("[INFO] gRPC WS API client initialized:", grpcEndpoint)
	return g.client, nil
}

func (g *grpcClient) authCtx(iamToken string) (context.Context, context.CancelFunc) {
	md := metadata.New(map[string]string{
		"authorization": "Bearer " + iamToken,
	})
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	return metadata.NewOutgoingContext(ctx, md), cancel
}

func (g *grpcClient) Send(connectionID string, data []byte, dataType string, iamToken string) error {
	client, err := g.ensure()
	if err != nil {
		return err
	}

	t := ws.SendToConnectionRequest_BINARY
	if dataType == "TEXT" {
		t = ws.SendToConnectionRequest_TEXT
	}

	ctx, cancel := g.authCtx(iamToken)
	defer cancel()
	_, err = client.Send(ctx, &ws.SendToConnectionRequest{
		ConnectionId: connectionID,
		Data:         data,
		Type:         t,
	})
	if err != nil {
		log.Printf("[WARN] wsapi.Send failed connId=%s bytes=%d err=%v", connectionID, len(data), err)
	}
	return err
}

func (g *grpcClient) Disconnect(connectionID string, iamToken string) error {
	client, err := g.ensure()
	if err != nil {
		return err
	}

	ctx, cancel := g.authCtx(iamToken)
	defer cancel()
	_, err = client.Disconnect(ctx, &ws.DisconnectRequest{
		ConnectionId: connectionID,
	})
	if err != nil {
		log.Printf("[WARN] wsapi.Disconnect failed connId=%s err=%v", connectionID, err)
	}
	return err
}
