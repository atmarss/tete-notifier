package notifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"time"
)

type Payload struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// TitlePrefix 通知标题前缀，用于在多个项目间快速区分消息来源。
const TitlePrefix = "【专属汽车管】"

// client 使用带超时的独立客户端，避免 http.Post 默认无超时导致协程永久阻塞。
var client = &http.Client{Timeout: 10 * time.Second}

// SendNotification 发送统一通知
func SendNotification(apiEndpoint, title, content string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("推送通知 panic: %v", r)
			log.Printf("推送通知 panic: %v\n%s", r, string(debug.Stack()))
		}
	}()
	payload := Payload{
		Title:   TitlePrefix + title,
		Content: content,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := client.Post(apiEndpoint, "application/json", bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("推送返回异常状态码: %d", resp.StatusCode)
	}
	return nil
}
