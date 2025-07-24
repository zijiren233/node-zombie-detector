package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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
	LastAlertTime       time.Time
	AlertCount          int // 告警次数统计
}

const (
	defaultCheckInterval  = 30 * time.Second
	defaultAlertThreshold = 3 * time.Minute
	defaultAlertInterval  = 10 * time.Minute
)

// Monitor K8s节点监控器
type Monitor struct {
	clusterName      string
	clientset        *kubernetes.Clientset
	metricsClientset *metricsclientset.Clientset
	nodeStatus       map[string]*NodeStatus
	feishuWebhook    string
	checkInterval    time.Duration
	alertThreshold   time.Duration
	alertInterval    time.Duration
	httpClient       *http.Client
}

type MonitorConfigFunc func(m *Monitor)

func WithCheckInterval(interval time.Duration) MonitorConfigFunc {
	return func(m *Monitor) {
		if interval <= 0 {
			return
		}

		m.checkInterval = interval
	}
}

func WithAlertThreshold(threshold time.Duration) MonitorConfigFunc {
	return func(m *Monitor) {
		if threshold <= 0 {
			return
		}

		m.alertThreshold = threshold
	}
}

func WithAlertInterval(interval time.Duration) MonitorConfigFunc {
	return func(m *Monitor) {
		if interval <= 0 {
			return
		}

		m.alertInterval = interval
	}
}

func WithClusterName(name string) MonitorConfigFunc {
	return func(m *Monitor) {
		m.clusterName = name
	}
}

// NewMonitor 创建新的监控器实例
func NewMonitor(
	config *rest.Config,
	feishuWebhook string,
	opts ...MonitorConfigFunc,
) (*Monitor, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	metricsClientset, err := metricsclientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics client: %w", err)
	}

	m := Monitor{
		clientset:        clientset,
		metricsClientset: metricsClientset,
		nodeStatus:       make(map[string]*NodeStatus),
		feishuWebhook:    feishuWebhook,
		checkInterval:    defaultCheckInterval,
		alertThreshold:   defaultAlertThreshold,
		alertInterval:    defaultAlertInterval,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}

	for _, opt := range opts {
		opt(&m)
	}

	return &m, nil
}

// Start 开始监控
func (m *Monitor) Start(ctx context.Context) {
	log.Println("Starting node monitor...")

	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	m.checkNodes(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping node monitor...")
			return
		case <-ticker.C:
			m.checkNodes(ctx)
		}
	}
}

// checkNodes 检查所有节点状态
func (m *Monitor) checkNodes(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	// 获取所有节点
	nodes, err := m.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Error listing nodes: %v", err)
		return
	}

	// 记录当前活跃节点
	activeNodes := make(map[string]bool)
	for _, node := range nodes.Items {
		activeNodes[node.Name] = true
	}

	// 清理已删除节点的状态
	m.cleanupDeletedNodes(activeNodes)

	nodeMetrics, err := m.metricsClientset.MetricsV1beta1().
		NodeMetricses().
		List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Error getting node metrics: %v", err)
		return
	}

	// 创建指标映射
	metricsMap := make(map[string]*v1beta1.NodeMetrics)
	if nodeMetrics != nil {
		for _, item := range nodeMetrics.Items {
			metricsMap[item.Name] = &item
		}
	}

	// 检查每个节点
	for _, node := range nodes.Items {
		// 只检查 Ready 状态的节点
		if !isNodeReady(&node) {
			log.Printf("Node %s is not ready, skipping", node.Name)
			continue
		}

		m.checkNode(ctx, &node, metricsMap[node.Name])
	}
}

// cleanupDeletedNodes 清理已删除节点的状态
func (m *Monitor) cleanupDeletedNodes(activeNodes map[string]bool) {
	for nodeName := range m.nodeStatus {
		if !activeNodes[nodeName] {
			delete(m.nodeStatus, nodeName)
			log.Printf("Cleaned up status for deleted node: %s", nodeName)
		}
	}
}

// getOrCreateNodeStatus 获取或创建节点状态（线程安全）
func (m *Monitor) getOrCreateNodeStatus(nodeName string) *NodeStatus {
	status, exists := m.nodeStatus[nodeName]
	if !exists {
		status = &NodeStatus{
			LastSeenWithMetrics: time.Now(),
			LastAlertTime:       time.Time{}, // 零值
			AlertCount:          0,
		}
		m.nodeStatus[nodeName] = status
	}

	return status
}

// checkNode 检查单个节点
func (m *Monitor) checkNode(
	ctx context.Context,
	node *v1.Node,
	metrics *v1beta1.NodeMetrics,
) {
	nodeName := node.Name
	hasMetrics := metrics != nil

	// 获取或创建节点状态
	status := m.getOrCreateNodeStatus(nodeName)

	// 如果节点有指标，更新最后看到指标的时间
	if hasMetrics {
		status.LastSeenWithMetrics = time.Now()

		log.Printf("Node %s: CPU=%s, Memory=%s",
			nodeName,
			metrics.Usage.Cpu().String(),
			metrics.Usage.Memory().String())
	} else {
		// 只有在 API 正常但节点没有指标时才记录
		log.Printf("Node %s: Metrics=<unknown> (API is working)", nodeName)
	}

	// 检查是否需要告警（只在 metrics API 正常工作时）
	if !hasMetrics {
		timeSinceLastMetrics := time.Since(status.LastSeenWithMetrics)
		timeSinceLastAlert := time.Since(status.LastAlertTime)

		if timeSinceLastMetrics >= m.alertThreshold {
			// 避免重复告警
			if status.LastAlertTime.IsZero() || timeSinceLastAlert >= m.alertInterval {
				m.sendAlert(ctx, nodeName, timeSinceLastMetrics)

				status.LastAlertTime = time.Now()
				status.AlertCount++
			}
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

// FeishuMessage 飞书消息结构
type FeishuMessage struct {
	MsgType string         `json:"msg_type"`
	Content map[string]any `json:"content"`
}

const alertFormat = "⚠️ **节点监控告警**\n\n" +
	"**节点名称**: %s\n" +
	"**问题描述**: 节点状态为 Ready，但已经 %v 没有资源指标数据\n" +
	"**可能原因**: \n" +
	"  - 节点上的 kubelet 可能已经卡死\n" +
	"  - 节点与 metrics-server 通信异常\n" +
	"  - 节点资源耗尽导致无法上报指标\n" +
	"**建议操作**: \n" +
	"  1. 检查节点 kubelet 状态\n" +
	"  2. 查看节点系统日志\n" +
	"  3. 考虑重启节点或迁移工作负载\n" +
	"**告警时间**: %s\n" +
	"**告警次数**: 第 %d 次"

const alertFormatWithCluster = "⚠️ **节点监控告警**\n\n" +
	"**集群名称**: %s\n" +
	"**节点名称**: %s\n" +
	"**问题描述**: 节点状态为 Ready，但已经 %v 没有资源指标数据\n" +
	"**可能原因**: \n" +
	"  - 节点上的 kubelet 可能已经卡死\n" +
	"  - 节点与 metrics-server 通信异常\n" +
	"  - 节点资源耗尽导致无法上报指标\n" +
	"**建议操作**: \n" +
	"  1. 检查节点 kubelet 状态\n" +
	"  2. 查看节点系统日志\n" +
	"  3. 考虑重启节点或迁移工作负载\n" +
	"**告警时间**: %s\n" +
	"**告警次数**: 第 %d 次"

// sendAlert 发送飞书告警
func (m *Monitor) sendAlert(ctx context.Context, nodeName string, duration time.Duration) {
	alertCount := m.nodeStatus[nodeName].AlertCount + 1

	var message string
	if m.clusterName != "" {
		message = fmt.Sprintf(
			alertFormatWithCluster,
			m.clusterName,
			nodeName,
			duration.Round(time.Second),
			time.Now().Format(time.DateTime),
			alertCount,
		)
	} else {
		message = fmt.Sprintf(
			alertFormat,
			nodeName,
			duration.Round(time.Second),
			time.Now().Format(time.DateTime),
			alertCount,
		)
	}

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

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		m.feishuWebhook,
		bytes.NewReader(jsonData),
	)
	if err != nil {
		log.Printf("Error creating http request: %v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		log.Printf("Error sending feishu alert: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body bytes.Buffer

		_, err := body.ReadFrom(resp.Body)
		if err != nil {
			log.Printf("Error reading feishu response body: %v", err)
		}

		log.Printf("Feishu webhook returned status code: %d, body: %s",
			resp.StatusCode, body.String())
	} else {
		log.Printf("Alert sent successfully for node %s (alert #%d)", nodeName, alertCount)
	}
}

// handleSignals 处理系统信号
func handleSignals(cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal: %v", sig)
		cancel()
	}()
}

func getEnvDuration(key string) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}

		log.Printf("Invalid duration for %s: %s", key, value)
	}

	return 0
}

func main() {
	// 设置日志格式
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// 从环境变量获取飞书 webhook URL
	feishuWebhook := os.Getenv("FEISHU_WEBHOOK_URL")
	if feishuWebhook == "" {
		log.Fatal("FEISHU_WEBHOOK_URL environment variable is required")
	}

	// 使用 in-cluster 配置（用于 ServiceAccount）
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to create in-cluster config: %v", err)
	}

	// 创建监控器
	monitor, err := NewMonitor(
		config,
		feishuWebhook,
		WithClusterName(os.Getenv("CLUSTER_NAME")),
		WithCheckInterval(getEnvDuration("CHECK_INTERVAL")),
		WithAlertThreshold(getEnvDuration("ALERT_THRESHOLD")),
		WithAlertInterval(getEnvDuration("ALERT_INTERVAL")),
	)
	if err != nil {
		log.Fatalf("Failed to create monitor: %v", err)
	}

	// 创建可取消的上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 处理系统信号
	handleSignals(cancel)

	// 启动监控
	log.Println("Node zombie detector starting...")
	monitor.Start(ctx)
	log.Println("Node zombie detector stopped")
}
