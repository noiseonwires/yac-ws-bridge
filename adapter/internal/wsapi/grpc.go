package wsapi

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	ws "github.com/yandex-cloud/go-genproto/yandex/cloud/serverless/apigateway/websocket/v1"
)

const grpcEndpoint = "apigateway-connections.api.cloud.yandex.net:443"

type grpcClient struct {
	once   sync.Once
	client ws.ConnectionServiceClient
	conn   *grpc.ClientConn
	err    error
}

func (g *grpcClient) init() {
	g.once.Do(func() {
		creds := credentials.NewTLS(&tls.Config{})
		g.conn, g.err = grpc.NewClient(grpcEndpoint, grpc.WithTransportCredentials(creds))
		if g.err != nil {
			g.err = fmt.Errorf("grpc dial: %w", g.err)
			return
		}
		g.client = ws.NewConnectionServiceClient(g.conn)
		log.Println("gRPC WS API client initialized:", grpcEndpoint)
	})
}

func (g *grpcClient) authCtx(iamToken string) context.Context {
	md := metadata.New(map[string]string{
		"authorization": "Bearer " + iamToken,
	})
	return metadata.NewOutgoingContext(context.Background(), md)
}

func (g *grpcClient) Send(connectionId string, data []byte, dataType string, iamToken string) error {
	g.init()
	if g.err != nil {
		return g.err
	}

	t := ws.SendToConnectionRequest_BINARY
	if dataType == "TEXT" {
		t = ws.SendToConnectionRequest_TEXT
	}

	_, err := g.client.Send(g.authCtx(iamToken), &ws.SendToConnectionRequest{
		ConnectionId: connectionId,
		Data:         data,
		Type:         t,
	})
	if err != nil {
		log.Println("wsapi.Send gRPC failed:", connectionId, err)
		return err
	}
	return nil
}

func (g *grpcClient) Disconnect(connectionId string, iamToken string) error {
	g.init()
	if g.err != nil {
		return g.err
	}

	_, err := g.client.Disconnect(g.authCtx(iamToken), &ws.DisconnectRequest{
		ConnectionId: connectionId,
	})
	if err != nil {
		log.Println("wsapi.Disconnect gRPC failed:", connectionId, err)
	}
	return err
}
