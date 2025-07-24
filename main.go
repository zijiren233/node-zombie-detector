package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
)

// NodeStatus 存储节点状态信息
type NodeStatus struct {
	LastSeenWithMetrics time.Time
}

// FeishuMessage 飞书消息结构
type FeishuMessage struct {
	MsgType string         `json:"msg_type"`
	Content map[string]any `json:"content"`
}

// Monitor K8s节点监控器
type Monitor struct {
	clientset        *kubernetes.Clientset
	metricsClientset *metricsclientset.Clientset
	nodeStatus       map[string]*NodeStatus
	feishuWebhook    string
	checkInterval    time.Duration
	alertThreshold   time.Duration
}

// NewMonitor 创建新的监控器实例
func NewMonitor(feishuWebhook string) (*Monitor, error) {
	// 使用 in-cluster 配置（用于 ServiceAccount）
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create in-cluster config: %w", err)
	}

	// 创建 kubernetes 客户端
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// 创建 metrics 客户端
	metricsClientset, err := metricsclientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics client: %w", err)
	}

	return &Monitor{
		clientset:        clientset,
		metricsClientset: metricsClientset,
		nodeStatus:       make(map[string]*NodeStatus),
		feishuWebhook:    feishuWebhook,
		checkInterval:    30 * time.Second, // 每30秒检查一次
		alertThreshold:   3 * time.Minute,  // 3分钟阈值
	}, nil
}

// Start 开始监控
func (m *Monitor) Start(ctx context.Context) {
	log.Println("Starting node monitor...")

	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	// 立即执行一次检查
	m.checkNodes()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping node monitor...")
			return
		case <-ticker.C:
			m.checkNodes()
		}
	}
}

// checkNodes 检查所有节点状态
func (m *Monitor) checkNodes() {
	// 获取所有节点
	nodes, err := m.clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Printf("Error listing nodes: %v", err)
		return
	}

	// 获取节点指标
	nodeMetrics, err := m.metricsClientset.MetricsV1beta1().
		NodeMetricses().
		List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Printf("Error getting node metrics: %v", err)
		// 继续处理，因为可能只是 metrics-server 暂时不可用
	}

	// 创建指标映射
	var metricsMap map[string]*v1beta1.NodeMetrics
	if nodeMetrics != nil {
		metricsMap = make(map[string]*v1beta1.NodeMetrics, len(nodeMetrics.Items))
		for _, item := range nodeMetrics.Items {
			metricsMap[item.Name] = &item
		}
	} else {
		metricsMap = make(map[string]*v1beta1.NodeMetrics)
	}

	// 检查每个节点
	for _, node := range nodes.Items {
		// only check ready nodes
		if !isNodeReady(&node) {
			continue
		}

		m.checkNode(&node, metricsMap[node.Name])
	}
}

// checkNode 检查单个节点
func (m *Monitor) checkNode(node *v1.Node, metrics *v1beta1.NodeMetrics) {
	nodeName := node.Name
	hasMetrics := metrics != nil

	// 获取或创建节点状态
	status, exists := m.nodeStatus[nodeName]
	if !exists {
		status = &NodeStatus{
			LastSeenWithMetrics: time.Now(),
		}
		m.nodeStatus[nodeName] = status
	}

	// 如果节点有指标，更新最后看到指标的时间
	if hasMetrics {
		status.LastSeenWithMetrics = time.Now()

		log.Printf("Node %s: CPU=%s, Memory=%s",
			nodeName,
			metrics.Usage.Cpu().String(),
			metrics.Usage.Memory().String())
	} else {
		log.Printf("Node %s: Metrics=<unknown>", nodeName)
	}

	// 检查是否需要告警
	if !hasMetrics {
		timeSinceLastMetrics := time.Since(status.LastSeenWithMetrics)
		if timeSinceLastMetrics >= m.alertThreshold {
			m.sendAlert(nodeName, timeSinceLastMetrics)
		}
	}
}

// isNodeReady 检查节点是否处于 Ready 状态
func isNodeReady(node *v1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == v1.NodeReady {
			return condition.Status == v1.ConditionTrue
		}
	}

	return false
}

const alertFormat = "⚠️ **节点监控告警**\n\n" +
	"**节点名称**: %s\n" +
	"**问题描述**: 节点状态为 Ready，但已经 %v 没有资源指标数据\n" +
	"**可能原因**: 节点可能已经卡死\n" +
	"**建议操作**: 请立即检查节点状态\n" +
	"**告警时间**: %s"

// sendAlert 发送飞书告警
func (m *Monitor) sendAlert(nodeName string, duration time.Duration) {
	message := fmt.Sprintf(
		alertFormat,
		nodeName,
		duration.Round(time.Second),
		time.Now().Format(time.DateTime),
	)

	feishuMsg := FeishuMessage{
		MsgType: "text",
		Content: map[string]any{
			"text": message,
		},
	}

	jsonData, err := json.Marshal(feishuMsg)
	if err != nil {
		log.Printf("Error marshaling feishu message: %v", err)
		return
	}

	r, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		m.feishuWebhook,
		bytes.NewReader(jsonData),
	)
	if err != nil {
		log.Printf("Error creating http request: %v", err)
		return
	}

	r.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		log.Printf("Error sending feishu alert: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Feishu webhook returned status code: %d", resp.StatusCode)
	} else {
		log.Printf("Alert sent for node %s", nodeName)
	}
}

func main() {
	// 从环境变量获取飞书 webhook URL
	feishuWebhook := os.Getenv("FEISHU_WEBHOOK_URL")
	if feishuWebhook == "" {
		log.Fatal("FEISHU_WEBHOOK_URL environment variable is required")
	}

	// 创建监控器
	monitor, err := NewMonitor(feishuWebhook)
	if err != nil {
		log.Fatalf("Failed to create monitor: %v", err)
	}

	// 启动监控
	ctx := context.Background()
	monitor.Start(ctx)
}
