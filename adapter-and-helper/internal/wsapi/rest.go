package wsapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

const restAPIBase = "https://apigateway-connections.api.cloud.yandex.net/apigateways/websocket/v1/connections"

type restClient struct{}

func (r *restClient) Send(connectionId string, data []byte, dataType string, iamToken string) error {
	b64 := base64.StdEncoding.EncodeToString(data)
	body, _ := json.Marshal(map[string]string{
		"data": b64,
		"type": dataType,
	})

	url := fmt.Sprintf("%s/%s:send", restAPIBase, connectionId)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+iamToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("wsSend: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		log.Printf("wsapi.Send REST failed: status=%d connId=%s body=%s", resp.StatusCode, connectionId, string(respBody))
		return fmt.Errorf("wsSend status %d for %s", resp.StatusCode, connectionId)
	}
	return nil
}

func (r *restClient) Disconnect(connectionId string, iamToken string) error {
	url := fmt.Sprintf("%s/%s:disconnect", restAPIBase, connectionId)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+iamToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return nil
}
