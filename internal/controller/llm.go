package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	LLMApiURL = "https://llm.glowbyteconsulting.com/api/chat/completions"
	LLMModel  = "anthropic.claude-sonnet-4-20250514"
)

// PodResourceInfo represents resource information for a pod
type PodResourceInfo struct {
	Name          string  `json:"name"`
	CPU           int     `json:"cpu"`
	Memory        int     `json:"memory"`
	CPUUsage      string  `json:"cpuUsage,omitempty"`
	MemoryUsage   string  `json:"memoryUsage,omitempty"`
	CPUPercentage float64 `json:"cpuPercentage,omitempty"`
	MemPercentage float64 `json:"memPercentage,omitempty"`
	Status        string  `json:"status,omitempty"`
}

// LLMRequest represents the request structure for the LLM API
type LLMRequest struct {
	Model    string       `json:"model"`
	Messages []LLMMessage `json:"messages"`
	Stream   bool         `json:"stream"`
}

// LLMMessage represents a message in the LLM conversation
type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLMResponse represents the response from the LLM API
type LLMResponse struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Object  string `json:"object"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Index        int    `json:"index"`
		Message      struct {
			Content string `json:"content"`
			Role    string `json:"role"`
		} `json:"message"`
	} `json:"choices"`
}

// ResourceRecommendationResponse represents the response with resource recommendations
type ResourceRecommendationResponse struct {
	Message    string                       `json:"message"`
	Namespaces map[string][]PodResourceInfo `json:"namespaces"`
}

// handleResourceAnalysisRequest handles the request to analyze pod resource usage
func (r *KubedeckReconciler) handleResourceAnalysisRequest(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	log := webServerLog.WithName("handleResourceAnalysisRequest")

	// Collect pod resource data
	podResourceData, err := r.collectPodResourceData(ctx)
	if err != nil {
		log.Error(err, "Failed to collect pod resource data")
		http.Error(w, "Failed to collect pod resource data: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Send data to LLM for analysis
	recommendation, err := r.getLLMResourceRecommendations(ctx, podResourceData)
	if err != nil {
		log.Error(err, "Failed to get LLM recommendations")
		http.Error(w, "Failed to get LLM recommendations: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Return the recommendation
	writeJsonResponse(w, recommendation, "resource recommendations", nil)
}

// collectPodResourceData collects resource data for all pods in the cluster
func (r *KubedeckReconciler) collectPodResourceData(ctx context.Context) (map[string][]PodResourceInfo, error) {
	// Get all pods
	var podList corev1.PodList
	if err := r.Client.List(ctx, &podList, &client.ListOptions{}); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Get metrics client
	metricsClient, err := metricsv.NewForConfig(r.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics client: %w", err)
	}

	// Get pod metrics
	podMetricsList, err := metricsClient.MetricsV1beta1().PodMetricses(corev1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod metrics: %w", err)
	}

	// Create a map of pod metrics for quick lookup
	podMetricsMap := make(map[string]v1beta1.PodMetrics)
	for _, metric := range podMetricsList.Items {
		key := fmt.Sprintf("%s/%s", metric.Namespace, metric.Name)
		podMetricsMap[key] = metric
	}

	// Organize pod data by namespace
	result := make(map[string][]PodResourceInfo)

	for _, pod := range podList.Items {
		// Skip pods that are not Running
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		// Calculate total CPU and memory limits
		var totalCPULimit, totalMemLimit resource.Quantity

		for _, container := range pod.Spec.Containers {
			if cpu, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
				totalCPULimit.Add(cpu)
			}
			if mem, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
				totalMemLimit.Add(mem)
			}
		}

		// Convert CPU millicores and memory to reasonable units
		cpuMillicores := totalCPULimit.MilliValue()
		memoryMi := totalMemLimit.Value() / (1024 * 1024) // Convert to Mi

		// Create pod resource info
		podInfo := PodResourceInfo{
			Name:   pod.Name,
			CPU:    int(cpuMillicores),
			Memory: int(memoryMi),
		}

		// Add usage data if available
		metricKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		if metric, exists := podMetricsMap[metricKey]; exists {
			var totalCPUUsage, totalMemUsage resource.Quantity

			for _, container := range metric.Containers {
				totalCPUUsage.Add(container.Usage.Cpu().DeepCopy())
				totalMemUsage.Add(container.Usage.Memory().DeepCopy())
			}

			// Calculate usage percentages
			if !totalCPULimit.IsZero() {
				podInfo.CPUPercentage = (totalCPUUsage.AsApproximateFloat64() / totalCPULimit.AsApproximateFloat64()) * 100
				podInfo.CPUUsage = totalCPUUsage.String()
			}

			if !totalMemLimit.IsZero() {
				podInfo.MemPercentage = (totalMemUsage.AsApproximateFloat64() / totalMemLimit.AsApproximateFloat64()) * 100
				podInfo.MemoryUsage = totalMemUsage.String()
			}
		}

		// Add to result
		if _, exists := result[pod.Namespace]; !exists {
			result[pod.Namespace] = []PodResourceInfo{}
		}
		result[pod.Namespace] = append(result[pod.Namespace], podInfo)
	}

	return result, nil
}

// getLLMResourceRecommendations sends pod resource data to the LLM and gets recommendations
func (r *KubedeckReconciler) getLLMResourceRecommendations(ctx context.Context, podResourceData map[string][]PodResourceInfo) (*ResourceRecommendationResponse, error) {
	log := webServerLog.WithName("getLLMResourceRecommendations")

	// Create the prompt for the LLM
	prompt := createLLMPrompt(podResourceData)

	// Create the LLM request
	llmReq := LLMRequest{
		Model: LLMModel,
		Messages: []LLMMessage{
			{
				Role:    "user",
				Content: prompt,
			},
		},
		Stream: false,
	}

	// Marshal the request
	reqBody, err := json.Marshal(llmReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal LLM request: %w", err)
	}

	// Create HTTP request with timeout
	httpCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(httpCtx, "POST", LLMApiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer sk-2511d8de29004787aff3b7f83bab43db")

	// Send the request
	log.Info("Sending request to LLM API", "url", LLMApiURL)
	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to LLM API: %w", err)
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		var errorResponse map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&errorResponse); err != nil {
			return nil, fmt.Errorf("LLM API returned status %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("LLM API returned status %d: %v", resp.StatusCode, errorResponse)
	}

	// Parse the response
	var llmResp LLMResponse
	if err := json.NewDecoder(resp.Body).Decode(&llmResp); err != nil {
		return nil, fmt.Errorf("failed to decode LLM response: %w", err)
	}

	// Check if the response has at least one choice
	if len(llmResp.Choices) == 0 {
		return nil, fmt.Errorf("LLM response has no choices")
	}

	// Extract the content
	content := llmResp.Choices[0].Message.Content
	log.Info("Received response from LLM", "content", content)

	// Parse the LLM response to get the recommendations
	var recommendation ResourceRecommendationResponse
	if err := json.Unmarshal([]byte(content), &recommendation); err != nil {
		// If we can't parse the JSON directly, try to extract it from the content
		jsonStart := strings.Index(content, "{")
		jsonEnd := strings.LastIndex(content, "}")
		if jsonStart != -1 && jsonEnd != -1 && jsonEnd > jsonStart {
			jsonContent := content[jsonStart : jsonEnd+1]
			if err := json.Unmarshal([]byte(jsonContent), &recommendation); err != nil {
				return nil, fmt.Errorf("failed to extract and parse recommendation JSON: %w", err)
			}
		} else {
			return nil, fmt.Errorf("failed to parse recommendation: %w", err)
		}
	}

	return &recommendation, nil
}

// createLLMPrompt creates the prompt for the LLM based on pod resource data
func createLLMPrompt(podResourceData map[string][]PodResourceInfo) string {
	sb := strings.Builder{}

	sb.WriteString("Проанализируйте следующие данные об использовании ресурсов подов Kubernetes и предоставьте рекомендации по корректировке ресурсов. ")
	sb.WriteString("Мне нужно, чтобы вы определили поды, которым выделено слишком много или слишком мало ресурсов, на основе их шаблонов использования. ")
	sb.WriteString("Пожалуйста, ответьте ТОЛЬКО в следующем формате JSON, без какого-либо другого текста:\n\n")
	sb.WriteString("```json\n")
	sb.WriteString("{\n")
	sb.WriteString("  \"message\": \"Ваш общий анализ и рекомендации здесь\",\n")
	sb.WriteString("  \"namespaces\": {\n")
	sb.WriteString("    \"имя-пространства-имен\": [\n")
	sb.WriteString("      {\n")
	sb.WriteString("        \"name\": \"имя-пода\",\n")
	sb.WriteString("        \"cpu\": 500,  // Рекомендуемый CPU в милликорах, или -1 если изменения не нужны\n")
	sb.WriteString("        \"memory\": 128,  // Рекомендуемая память в Mi, или -1 если изменения не нужны\n")
	sb.WriteString("        \"status\": \"warning\" или \"critical\"  // Уровень серьезности рекомендации\n")
	sb.WriteString("      }\n")
	sb.WriteString("    ]\n")
	sb.WriteString("  }\n")
	sb.WriteString("}\n")
	sb.WriteString("```\n\n")

	sb.WriteString("В вашем анализе учитывайте следующие рекомендации:\n")
	sb.WriteString("1. Если процент использования CPU постоянно ниже 30%, рассмотрите возможность рекомендации снижения лимитов CPU (status=warning).\n")
	sb.WriteString("2. Если процент использования CPU постоянно выше 80%, рассмотрите возможность рекомендации увеличения лимитов CPU (status=critical).\n")
	sb.WriteString("3. Если процент использования памяти постоянно ниже 40%, рассмотрите возможность рекомендации снижения лимитов памяти (status=warning).\n")
	sb.WriteString("4. Если процент использования памяти постоянно выше 75%, рассмотрите возможность рекомендации увеличения лимитов памяти (status=critical).\n")
	sb.WriteString("5. Если ресурс не требует корректировки, используйте -1 для этого значения ресурса.\n")
	sb.WriteString("6. Включайте только те поды, которые нуждаются в корректировке хотя бы одного ресурса.\n")
	sb.WriteString("7. Установите статус 'warning' для рекомендаций по уменьшению ресурсов или небольших проблем.\n")
	sb.WriteString("8. Установите статус 'critical' для рекомендаций по увеличению ресурсов или серьезных проблем.\n\n")

	sb.WriteString("Вот текущие данные о ресурсах по пространствам имен:\n\n")

	// Add pod resource data to the prompt
	for namespace, pods := range podResourceData {
		sb.WriteString(fmt.Sprintf("Пространство имен: %s\n", namespace))
		for _, pod := range pods {
			sb.WriteString(fmt.Sprintf("- Под: %s\n", pod.Name))
			sb.WriteString(fmt.Sprintf("  Лимит CPU: %dm, ", pod.CPU))
			if pod.CPUUsage != "" {
				sb.WriteString(fmt.Sprintf("Использование CPU: %s (%.1f%%)\n", pod.CPUUsage, pod.CPUPercentage))
			} else {
				sb.WriteString("Использование CPU: Неизвестно\n")
			}

			sb.WriteString(fmt.Sprintf("  Лимит памяти: %dMi, ", pod.Memory))
			if pod.MemoryUsage != "" {
				sb.WriteString(fmt.Sprintf("Использование памяти: %s (%.1f%%)\n", pod.MemoryUsage, pod.MemPercentage))
			} else {
				sb.WriteString("Использование памяти: Неизвестно\n")
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Верните ТОЛЬКО объект JSON, как указано выше, без маркировки markdown, объяснений или дополнительного текста до или после. Убедитесь, что ваш ответ является допустимым JSON, который можно разобрать напрямую. Ответ должен быть на русском языке.")

	return sb.String()
}
