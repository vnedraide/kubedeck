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

// TelegramBotConfig содержит настройки для Telegram-бота
type TelegramBotConfig struct {
	Token         string  `json:"token,omitempty"`         // Новый токен для бота (опционально)
	CheckInterval int     `json:"checkInterval,omitempty"` // Новый интервал проверки в секундах (опционально)
	ChatIDs       []int64 `json:"chatIDs,omitempty"`       // Новый список ID чатов для отправки уведомлений (опционально)
}

const (
	// Значения по умолчанию
	defaultTelegramBotToken = "7803977827:AAE8aSbXaiwDl2_nHVZBSTyws_VVgsGwrVE"
	defaultCheckInterval    = 2700 // 45 минут в секундах

	// AlertDeduplicationWindow - окно дедупликации алертов в часах
	AlertDeduplicationWindow = 4

	// WebUIBaseURL - базовый URL веб-интерфейса
	WebUIBaseURL = "https://yandex.ru/images/search?from=tabbar&img_url=https%3A%2F%2Ficdn.lenta.ru%2Fimages%2F2025%2F05%2F13%2F18%2F20250513184839314%2Foriginal_88202f632e59f19d8938a1a03e58c8c4.jpg&lr=213&pos=0&rpt=simage&text=окак"
)

// ChatIDs - список ID чатов для отправки уведомлений
var ChatIDs = []int64{
	-4835116305,
}

// TelegramBotSettings содержит настройки Telegram-бота
type TelegramBotSettings struct {
	sync.RWMutex
	token         string
	checkInterval int
	chatIDs       []int64
	active        bool
	stopChan      chan struct{}
}

// NewTelegramBotSettings создает новые настройки бота с значениями по умолчанию
func NewTelegramBotSettings() *TelegramBotSettings {
	return &TelegramBotSettings{
		token:         defaultTelegramBotToken,
		checkInterval: defaultCheckInterval,
		chatIDs:       append([]int64{}, ChatIDs...), // Копируем значения по умолчанию
		active:        false,
		stopChan:      make(chan struct{}),
	}
}

// GetToken возвращает текущий токен бота
func (s *TelegramBotSettings) GetToken() string {
	s.RLock()
	defer s.RUnlock()
	return s.token
}

// GetCheckInterval возвращает текущий интервал проверки в секундах
func (s *TelegramBotSettings) GetCheckInterval() int {
	s.RLock()
	defer s.RUnlock()
	return s.checkInterval
}

// GetChatIDs возвращает текущий список ID чатов
func (s *TelegramBotSettings) GetChatIDs() []int64 {
	s.RLock()
	defer s.RUnlock()
	// Возвращаем копию, чтобы избежать гонок данных
	result := make([]int64, len(s.chatIDs))
	copy(result, s.chatIDs)
	return result
}

// UpdateSettings обновляет настройки бота
func (s *TelegramBotSettings) UpdateSettings(config *TelegramBotConfig) bool {
	s.Lock()
	defer s.Unlock()

	changed := false

	// Обновляем токен, если он указан
	if config.Token != "" && config.Token != s.token {
		s.token = config.Token
		changed = true
	}

	// Обновляем интервал проверки, если он указан и больше 0
	if config.CheckInterval > 0 && config.CheckInterval != s.checkInterval {
		s.checkInterval = config.CheckInterval
		changed = true
	}

	// Обновляем список чатов, если он указан и не пустой
	if len(config.ChatIDs) > 0 {
		// Проверяем, изменился ли список чатов
		if !equalChatIDs(s.chatIDs, config.ChatIDs) {
			s.chatIDs = make([]int64, len(config.ChatIDs))
			copy(s.chatIDs, config.ChatIDs)
			changed = true
		}
	}

	// Если настройки изменились и бот активен, отправляем сигнал для остановки и перезапуска
	if changed && s.active {
		close(s.stopChan)
		s.stopChan = make(chan struct{})
	}

	return changed
}

// equalChatIDs проверяет, одинаковы ли два списка ID чатов
func equalChatIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
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

	// Маскируем токен для логов, показывая только первые 8 символов
	tokenMasked := "********"
	if token := r.TelegramBotSettings.GetToken(); len(token) > 8 {
		tokenMasked = token[:8] + "..."
	}

	log.Info("Starting Telegram bot for alerts",
		"token", tokenMasked,
		"checkInterval", r.TelegramBotSettings.GetCheckInterval(),
		"chatIDs", r.TelegramBotSettings.GetChatIDs())

	// Помечаем бота как активного
	r.TelegramBotSettings.Lock()
	r.TelegramBotSettings.active = true
	stopChan := r.TelegramBotSettings.stopChan
	r.TelegramBotSettings.Unlock()

	tracker := NewAlertTracker()

	// Периодически очищаем старые алерты
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-stopChan:
				log.Info("Stopping alert tracker cleaner")
				return
			case <-ticker.C:
				tracker.CleanupOldAlerts()
			}
		}
	}()

	// Периодически проверяем ресурсы и отправляем алерты
	go func() {
		// Получаем начальный интервал проверки
		checkInterval := r.TelegramBotSettings.GetCheckInterval()
		ticker := time.NewTicker(time.Second * time.Duration(checkInterval))
		defer ticker.Stop()

		// Немедленная первая проверка после запуска
		r.checkResourcesAndSendAlerts(ctx, tracker)

		for {
			select {
			case <-ctx.Done():
				return
			case <-stopChan:
				log.Info("Stopping resource checker")
				return
			case <-ticker.C:
				// Обновляем тикер с текущим интервалом проверки
				newCheckInterval := r.TelegramBotSettings.GetCheckInterval()
				if newCheckInterval != checkInterval {
					ticker.Reset(time.Second * time.Duration(newCheckInterval))
					checkInterval = newCheckInterval
					log.Info("Updated check interval", "newInterval", checkInterval)
				}
				r.checkResourcesAndSendAlerts(ctx, tracker)
			}
		}
	}()

	// Ожидаем сигнал о необходимости перезапуска
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-stopChan:
			log.Info("Restarting Telegram bot with new settings")
			// Небольшая задержка перед перезапуском
			time.Sleep(time.Second)
			r.StartTelegramBot(ctx)
			return
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
	token := r.TelegramBotSettings.GetToken()
	chatIDs := r.TelegramBotSettings.GetChatIDs()

	// Если список чатов пуст, используем глобальный список по умолчанию
	if len(chatIDs) == 0 {
		chatIDs = ChatIDs
	}

	for _, chatID := range chatIDs {
		err := sendTelegramSummaryMessage(chatID, message, token)
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
func sendTelegramSummaryMessage(chatID int64, text string, token string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

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

// handleTelegramBotConfigRequest обрабатывает запрос на обновление настроек Telegram-бота
func (r *KubedeckReconciler) handleTelegramBotConfigRequest(w http.ResponseWriter, req *http.Request) {
	log := webServerLog.WithName("telegram-bot-config")

	// Проверяем метод запроса
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Декодируем JSON-запрос
	var config TelegramBotConfig
	if err := json.NewDecoder(req.Body).Decode(&config); err != nil {
		log.Error(err, "Failed to decode request body")
		http.Error(w, "Invalid JSON request: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	// Проверяем, что хотя бы одно поле указано
	if config.Token == "" && config.CheckInterval <= 0 && len(config.ChatIDs) == 0 {
		http.Error(w, "At least one of token, checkInterval or chatIDs must be specified", http.StatusBadRequest)
		return
	}

	// Сохраняем текущие настройки для логирования
	oldToken := r.TelegramBotSettings.GetToken()
	oldInterval := r.TelegramBotSettings.GetCheckInterval()
	oldChatIDs := r.TelegramBotSettings.GetChatIDs()

	// Обновляем настройки бота
	changed := r.TelegramBotSettings.UpdateSettings(&config)

	// Маскируем токены для логов
	oldTokenMasked := "********"
	newTokenMasked := "********"

	if len(oldToken) > 8 {
		oldTokenMasked = oldToken[:8] + "..."
	}

	if newToken := r.TelegramBotSettings.GetToken(); len(newToken) > 8 {
		newTokenMasked = newToken[:8] + "..."
	}

	log.Info("Telegram bot settings update",
		"changed", changed,
		"tokenUpdated", config.Token != "",
		"oldToken", oldTokenMasked,
		"newToken", newTokenMasked,
		"intervalUpdated", config.CheckInterval > 0,
		"oldInterval", oldInterval,
		"newInterval", r.TelegramBotSettings.GetCheckInterval(),
		"chatIDsUpdated", len(config.ChatIDs) > 0,
		"oldChatIDs", oldChatIDs,
		"newChatIDs", r.TelegramBotSettings.GetChatIDs())

	// Возвращаем текущие настройки
	response := map[string]any{
		"success": true,
		"message": "Telegram bot settings updated successfully",
		"changed": changed,
		"settings": map[string]any{
			"tokenUpdated":    config.Token != "",
			"intervalUpdated": config.CheckInterval > 0,
			"chatIDsUpdated":  len(config.ChatIDs) > 0,
			"currentInterval": r.TelegramBotSettings.GetCheckInterval(),
			"currentChatIDs":  r.TelegramBotSettings.GetChatIDs(),
		},
	}

	// Записываем JSON-ответ
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Error(err, "Failed to encode response")
		http.Error(w, "Failed to encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
}
