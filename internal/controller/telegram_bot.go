package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// TelegramBotToken - токен Telegram бота
	TelegramBotToken = "5410005863:AAEkQsFcKe_V6cb0Cl4DugW_tX4334E7WtA"

	// CheckInterval - интервал проверки ресурсов в секундах
	CheckInterval = 2700 // 45 минут

	// AlertDeduplicationWindow - окно дедупликации алертов в часах
	AlertDeduplicationWindow = 4

	// WebUIBaseURL - базовый URL веб-интерфейса
	WebUIBaseURL = "https://yandex.ru/images/search?from=tabbar&img_url=https%3A%2F%2Ficdn.lenta.ru%2Fimages%2F2025%2F05%2F13%2F18%2F20250513184839314%2Foriginal_88202f632e59f19d8938a1a03e58c8c4.jpg&lr=213&pos=0&rpt=simage&text=окак"
)

// ChatIDs - список ID чатов для отправки уведомлений
var ChatIDs = []int64{
	-4835116305,
}

// TelegramMessage представляет сообщение для API Telegram
type TelegramMessage struct {
	ChatID      int64                `json:"chat_id"`
	Text        string               `json:"text"`
	ParseMode   string               `json:"parse_mode,omitempty"`
	ReplyMarkup *TelegramReplyMarkup `json:"reply_markup,omitempty"`
}

// TelegramReplyMarkup представляет клавиатуру с кнопками в Telegram
type TelegramReplyMarkup struct {
	InlineKeyboard [][]TelegramInlineButton `json:"inline_keyboard"`
}

// TelegramInlineButton представляет inline-кнопку в Telegram
type TelegramInlineButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// PodInfo содержит информацию о поде
type PodInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Другие поля, которые могут быть необходимы
}

// Recommendation содержит рекомендации по ресурсам
type Recommendation struct {
	Message    string               `json:"message"`
	Namespaces map[string][]PodInfo `json:"namespaces"`
}

// AlertTracker отслеживает отправленные алерты для предотвращения дублирования
type AlertTracker struct {
	sync.Mutex
	alerts map[string]time.Time // ключ: namespace/pod, значение: время последнего алерта
}

// NewAlertTracker создает новый трекер алертов
func NewAlertTracker() *AlertTracker {
	return &AlertTracker{
		alerts: make(map[string]time.Time),
	}
}

// ShouldSendAlert проверяет, нужно ли отправлять алерт для данного пода
func (a *AlertTracker) ShouldSendAlert(namespace, podName string) bool {
	a.Lock()
	defer a.Unlock()

	key := fmt.Sprintf("%s/%s", namespace, podName)
	lastAlert, exists := a.alerts[key]

	now := time.Now()
	// Если алерт не существует или прошло больше времени, чем окно дедупликации
	if !exists || now.Sub(lastAlert).Hours() >= AlertDeduplicationWindow {
		a.alerts[key] = now
		return true
	}

	return false
}

// CleanupOldAlerts удаляет старые алерты из трекера
func (a *AlertTracker) CleanupOldAlerts() {
	a.Lock()
	defer a.Unlock()

	threshold := time.Now().Add(-time.Hour * AlertDeduplicationWindow)
	for key, lastAlert := range a.alerts {
		if lastAlert.Before(threshold) {
			delete(a.alerts, key)
		}
	}
}

// StartTelegramBot запускает Telegram бота для отправки алертов
func (r *KubedeckReconciler) StartTelegramBot(ctx context.Context) {
	log := webServerLog.WithName("telegram-bot")
	log.Info("Starting Telegram bot for alerts")

	tracker := NewAlertTracker()

	// Периодически очищаем старые алерты
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tracker.CleanupOldAlerts()
			}
		}
	}()

	// Периодически проверяем ресурсы и отправляем алерты
	go func() {
		ticker := time.NewTicker(time.Second * CheckInterval)
		defer ticker.Stop()

		// Немедленная первая проверка после запуска
		r.checkResourcesAndSendAlerts(ctx, tracker)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.checkResourcesAndSendAlerts(ctx, tracker)
			}
		}
	}()
}

// checkResourcesAndSendAlerts проверяет ресурсы и отправляет ОДНО сообщение с краткой информацией
func (r *KubedeckReconciler) checkResourcesAndSendAlerts(ctx context.Context, tracker *AlertTracker) {
	log := webServerLog.WithName("resource-checker")

	// Получаем данные о ресурсах
	podResourceData, err := r.collectPodResourceData(ctx)
	if err != nil {
		log.Error(err, "Failed to collect pod resource data")
		return
	}

	// Отправляем данные на анализ в LLM
	recommendation, err := r.getLLMResourceRecommendations(ctx, podResourceData)
	if err != nil {
		log.Error(err, "Failed to get LLM recommendations")
		return
	}

	// Проверяем наличие проблемных подов
	hasProblematicPods := false
	totalProblematicPods := 0

	for _, pods := range recommendation.Namespaces {
		if len(pods) > 0 {
			hasProblematicPods = true
			totalProblematicPods += len(pods)
		}
	}

	if !hasProblematicPods {
		log.Info("No problematic pods found")
		return
	}

	// Формируем ОДНО сообщение со всеми алертами
	message := formatSummaryAlertMessage(recommendation, totalProblematicPods)

	// Отправляем одно сообщение во все чаты
	for _, chatID := range ChatIDs {
		err := sendTelegramSummaryMessage(chatID, message)
		if err != nil {
			log.Error(err, "Failed to send Telegram summary alert", "chatID", chatID)
		} else {
			log.Info("Sent Telegram summary alert", "chatID", chatID, "problematicPods", totalProblematicPods)
		}
	}
}

// formatSummaryAlertMessage форматирует краткое сообщение со всеми алертами
func formatSummaryAlertMessage(recommendation *ResourceRecommendationResponse, totalPods int) string {
	var sb strings.Builder

	// Заголовок сообщения
	sb.WriteString("*Kubernetes Resource Alert Summary*\n\n")
	sb.WriteString(fmt.Sprintf("Обнаружено *%d* проблемных подов\n\n", totalPods))

	// Добавляем информацию по каждому неймспейсу
	for namespace, pods := range recommendation.Namespaces {
		if len(pods) == 0 {
			continue
		}

		sb.WriteString(fmt.Sprintf("*Namespace:* `%s`\n", namespace))
		sb.WriteString(fmt.Sprintf("Проблемных подов: %d\n", len(pods)))

		// Добавляем статистику по статусам
		criticalCount := 0
		warningCount := 0
		infoCount := 0

		for _, pod := range pods {
			switch pod.Status {
			case "critical":
				criticalCount++
			case "warning":
				warningCount++
			default:
				infoCount++
			}
		}

		if criticalCount > 0 {
			sb.WriteString(fmt.Sprintf("🔴 Критических: %d\n", criticalCount))
		}
		if warningCount > 0 {
			sb.WriteString(fmt.Sprintf("⚠️ Предупреждений: %d\n", warningCount))
		}
		if infoCount > 0 {
			sb.WriteString(fmt.Sprintf("ℹ️ Информационных: %d\n", infoCount))
		}

		sb.WriteString("\n")
	}

	// Добавляем общую рекомендацию
	sb.WriteString("*Общая рекомендация:*\n")
	sb.WriteString(recommendation.Message)

	return sb.String()
}

// sendTelegramSummaryMessage отправляет общее сообщение в Telegram
func sendTelegramSummaryMessage(chatID int64, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TelegramBotToken)

	message := TelegramMessage{
		ChatID:    chatID,
		Text:      text,
		ParseMode: "Markdown",
		ReplyMarkup: &TelegramReplyMarkup{
			InlineKeyboard: [][]TelegramInlineButton{
				{
					{
						Text: "Открыть в Kubedeck",
						URL:  WebUIBaseURL,
					},
				},
			},
		},
	}

	jsonData, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal telegram message: %w", err)
	}

	// Отправляем запрос к API Telegram
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to send telegram message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errorResponse map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&errorResponse); err != nil {
			return fmt.Errorf("telegram API error: status code %d", resp.StatusCode)
		}
		return fmt.Errorf("telegram API error: %v", errorResponse)
	}

	return nil
}
