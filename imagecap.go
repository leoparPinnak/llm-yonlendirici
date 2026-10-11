package main

import (
	"encoding/json"
)

// defaultMaxImagesPerRequest, upstream'ın (free havuz) tek istekte kabul ettiği
// görsel sınırıdır. Aşan istek 400 "At most 8 image(s) may be provided in one
// prompt" ile geri döner ve görselli sohbet kilitlenir: istemci tarihçeyi her
// tur yeniden gönderdiği için hata bir kez değil, ta ki istekteki görsel sayısı
// yeniden sınırın altına inene kadar HER TURDA tekrar eder.
const defaultMaxImagesPerRequest = 8

// imageDropPlaceholder, sınırdan çıkarılan eski görselin yerine yazılan metin.
// Görselin kendisi gider; model "burada bir görsel vardı" bilgisini korur.
const imageDropPlaceholder = "[image dropped by gateway: request exceeded the " +
	"image limit; this older image was removed to keep the newest ones]"

// imageProbe, bir content parçasının tipini okumak için gereken en küçük alan
// seti. Yalnızca okuma amaçlıdır; parçalar asla bu yapıyla yeniden yazılmaz.
type imageProbe struct {
	Type string `json:"type"`
}

// chatModelName, chat gövdesindeki model adını okur; okunamazsa boş döner.
// Görsel sınırı model bazlı çalıştığı için sınır belirlenmeden önce okunur.
func chatModelName(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// isImagePart reports whether one raw OpenAI content part carries an image.
// Uyumlu sunucular görseli "image_url" ile, bazıları "input_image" ile
// gönderir; ikisi de sınıra girer.
func isImagePart(raw json.RawMessage) bool {
	var probe imageProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.Type == "image_url" || probe.Type == "input_image"
}

// capImagesInChatBody, chat-completions gövdesindeki görsel sayısını limit ile
// sınırlar.
//
// Üst düzey alanlar (model, tools, temperature, stream_options, ...) ham
// baytlarıyla korunur; yalnızca görsel taşıyan mesajların "content" dizisi
// yeniden yazılır ve o dizide de yalnızca KALDIRILAN parçalar değişir —
// diğer parçalar bayt bayt aynı kalır. Sınır aşıldığında EN ESKİ görseller
// metin yer tutucusuyla değiştirilir, en yenileri olduğu gibi kalır: modelin
// güncel görsel bağlamı korunur, geçmişte bir kez incelediği görsel tekrar
// yüklenmez ve istek asla upstream'in görsel sınırını aşmaz.
//
// @param body - ham istek gövdesi.
// @param limit - istek başına izin verilen görsel sayısı; 0/negatif işlem yapmaz.
// @returns (a) upstream'e gidecek gövde (işlem yoksa bayt bayt aynı),
//
//	(b) sınırdan çıkarılan görsel sayısı,
//	(c) gövde JSON değilse ayrıştırma hatası — çağrımcı gövdeyi DEĞİŞTİRMEMELİ.
func capImagesInChatBody(body []byte, limit int) ([]byte, int, error) {
	if limit <= 0 || len(body) == 0 {
		return body, 0, nil
	}

	// Üst düzey: ham değerleri olduğu gibi taşıyan kayıpsız harita.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, 0, err
	}
	rawMessages, ok := top["messages"]
	if !ok {
		return body, 0, nil
	}

	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return body, 0, err
	}

	type slot struct{ message, part int }
	var slots []slot
	// Mesaj başına ham content dizisi; string content'te nil kalır.
	contents := make([][]json.RawMessage, len(messages))

	for i, message := range messages {
		rawContent, ok := message["content"]
		if !ok || len(rawContent) == 0 {
			continue
		}
		var parts []json.RawMessage
		if err := json.Unmarshal(rawContent, &parts); err != nil {
			// content bir string (düz metin mesajı) → görsel içermez.
			continue
		}
		contents[i] = parts
		for j, part := range parts {
			if isImagePart(part) {
				slots = append(slots, slot{message: i, part: j})
			}
		}
	}

	if len(slots) <= limit {
		return body, 0, nil
	}

	// En eskilerinden başlayarak fazlalık kadarını kaldır; en yenileri kalır.
	replacement := []byte(`{"type":"text","text":` + jsonString(imageDropPlaceholder) + `}`)
	drop := slots[:len(slots)-limit]
	touched := make(map[int]bool, len(drop))
	for _, s := range drop {
		contents[s.message][s.part] = replacement
		touched[s.message] = true
	}

	for i := range touched {
		encoded, err := json.Marshal(contents[i])
		if err != nil {
			return body, 0, err
		}
		messages[i]["content"] = encoded
	}

	encodedMessages, err := json.Marshal(messages)
	if err != nil {
		return body, 0, err
	}
	top["messages"] = encodedMessages

	encoded, err := json.Marshal(top)
	if err != nil {
		return body, 0, err
	}
	return encoded, len(drop), nil
}

// jsonString, bir metni JSON dizgesine çevirir.
func jsonString(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(b)
}
