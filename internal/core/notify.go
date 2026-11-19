package core

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"time"
)

// Notify sends an event ("success" | "failure") to the configured channels. Never blocks a job.
func (a *App) Notify(event, text string) {
	if event == "success" && a.Store.Setting("notify_success") != "1" {
		return
	}
	hook := a.Store.Setting("webhook_url")
	chat := a.Store.Setting("telegram_chat")
	var token string
	if sealed := a.Store.Setting("telegram_token"); sealed != "" {
		if raw, err := hexDecode(sealed); err == nil {
			if b, err := a.Keys.Open(raw); err == nil {
				token = string(b)
			}
		}
	}
	go func() {
		cl := &http.Client{Timeout: 15 * time.Second}
		if hook != "" {
			body, _ := json.Marshal(map[string]string{"event": event, "text": text})
			if resp, err := cl.Post(hook, "application/json", bytes.NewReader(body)); err != nil {
				log.Printf("notify webhook: %v", err)
			} else {
				resp.Body.Close()
			}
		}
		if token != "" && chat != "" {
			resp, err := cl.PostForm("https://api.telegram.org/bot"+token+"/sendMessage", url.Values{"chat_id": {chat}, "text": {text}})
			if err != nil {
				log.Printf("notify telegram: request failed") // the error text would contain the token
			} else {
				resp.Body.Close()
			}
		}
	}()
}
