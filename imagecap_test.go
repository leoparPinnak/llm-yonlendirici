package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// chatBodyWithImages, n görselli bir chat-completions gövdesi üretir.
func chatBodyWithImages(n int) []byte {
	messages := make([]map[string]any, 0, n+1)
	messages = append(messages, map[string]any{
		"role":    "system",
		"content": "sen bir asistansın",
	})
	for i := 0; i < n; i++ {
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "bak " + string(rune('A'+i))},
				{
					"type": "image_url",
					"image_url": map[string]any{
						"url": "data:image/png;base64,AAAA" + strings.Repeat("B", i+1),
					},
				},
			},
		})
	}
	body, err := json.Marshal(map[string]any{
		"model":       "mimo-v2.6-pro",
		"stream":      true,
		"temperature": 0.7,
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}},
		},
		"messages": messages,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// imageCount, bir gövdede kalan image_url parçasını sayar. String content'li
// mesajlar (ör. system) atlanır — onlar görsel taşıyamaz.
func imageCount(t *testing.T, body []byte) int {
	t.Helper()
	var top struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("çıktı JSON değil: %v", err)
	}
	count := 0
	for _, m := range top.Messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "image_url" || p.Type == "input_image" {
				count++
			}
		}
	}
	return count
}

// placeholderCount, yer tutucu metnin kaç kez yazıldığını sayar.
func placeholderCount(body []byte) int {
	return strings.Count(string(body), imageDropPlaceholder)
}

func TestCapImagesKeepsNewestAndDropsOldest(t *testing.T) {
	in := chatBodyWithImages(10)
	out, dropped, err := capImagesInChatBody(in, 8)
	if err != nil {
		t.Fatalf("capImagesInChatBody hata: %v", err)
	}
	if dropped != 2 {
		t.Fatalf("düşen görsel 2 olmalı, %d geldi", dropped)
	}
	if got := imageCount(t, out); got != 8 {
		t.Fatalf("sınır sonrası görsel 8 olmalı, %d geldi", got)
	}
	if got := placeholderCount(out); got != 2 {
		t.Fatalf("yer tutucu 2 kez olmalı, %d geldi", got)
	}

	// En yeniler korunmalı: son kullanıcının görseli (en çok B tekrarı) duruyor.
	if !strings.Contains(string(out), strings.Repeat("B", 10)) {
		t.Fatal("en yeni görsel kaldırıldı; oysa en eskiler düşmeli")
	}
	if strings.Contains(string(in[:len(in)-32]), "dropped by gateway") {
		t.Fatal("girdi değiştirilmiş olmamalı")
	}
}

func TestCapImagesBelowLimitLeavesBodyByteIdentical(t *testing.T) {
	in := chatBodyWithImages(8)
	out, dropped, err := capImagesInChatBody(in, 8)
	if err != nil {
		t.Fatalf("hata: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("sınırdan 0 düşmeliydi, %d düştü", dropped)
	}
	if string(out) != string(in) {
		t.Fatal("sınırsız istek bayt bayt aynı kalmalı")
	}
}

func TestCapImagesPreservesTopLevelFields(t *testing.T) {
	in := chatBodyWithImages(9)
	out, dropped, err := capImagesInChatBody(in, 8)
	if err != nil {
		t.Fatalf("hata: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("1 düşmeliydi, %d düştü", dropped)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("üst düzey JSON bozuk: %v", err)
	}
	for _, key := range []string{"model", "stream", "temperature", "tools", "messages"} {
		if _, ok := top[key]; !ok {
			t.Fatalf("üst düzey alan kayboldu: %s", key)
		}
	}
	if !strings.Contains(string(top["model"]), "mimo-v2.6-pro") {
		t.Fatalf("model değeri değişti: %s", top["model"])
	}
	if !strings.Contains(string(top["tools"]), `"name":"read"`) {
		t.Fatalf("tools şeması bozuldu: %s", top["tools"])
	}
}

func TestCapImagesStringValueContentUntouched(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"düz metin"}]}`)
	out, dropped, err := capImagesInChatBody(body, 8)
	if err != nil {
		t.Fatalf("hata: %v", err)
	}
	if dropped != 0 || string(out) != string(body) {
		t.Fatalf("string content değişmemeli (dropped=%d)", dropped)
	}
}

func TestCapImagesNoMessagesOrBadJSON(t *testing.T) {
	body := []byte(`{"model":"x","input":"gorsel yok"}`)
	out, dropped, err := capImagesInChatBody(body, 8)
	if err != nil || dropped != 0 || string(out) != string(body) {
		t.Fatalf("messages'sız gövde aynen kalmalı (err=%v dropped=%d)", err, dropped)
	}

	broken := []byte(`{gecersiz`)
	out, dropped, err = capImagesInChatBody(broken, 8)
	if err == nil {
		t.Fatal("bozuk JSON'da hata dönmeli")
	}
	if string(out) != string(broken) {
		t.Fatal("hata durumunda gövde aynen geri dönmeli")
	}
	if dropped != 0 {
		t.Fatalf("hata durumunda 0 düşmeli, %d", dropped)
	}

	// limit 0 → devre dışı
	in := chatBodyWithImages(20)
	out, dropped, err = capImagesInChatBody(in, 0)
	if err != nil || dropped != 0 || string(out) != string(in) {
		t.Fatal("limit 0 işlemi devre dışı bırakmalı")
	}
}

func TestCapImagesInputImagePartsAlsoCount(t *testing.T) {
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":[
			{"type":"input_image","image_url":{"url":"data:image/png;base64,QQQ"}},
			{"type":"text","text":"bir"}
		]}
	]}`)
	out, dropped, err := capImagesInChatBody(body, 0)
	if err != nil {
		t.Fatalf("hata: %v", err)
	}
	if dropped != 0 || string(out) != string(body) {
		t.Fatal("limit 0 ile değişmemeli")
	}
	// limit 1 sınırında 1 görsel → düşmemeli
	out, dropped, err = capImagesInChatBody(body, 1)
	if err != nil || dropped != 0 {
		t.Fatalf("1 görsel limit içinde kalmalı (dropped=%d err=%v)", dropped, err)
	}
}
