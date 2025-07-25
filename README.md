# node-zombie-detector

一个 Kubernetes 节点监控工具，用于检测处于 Ready 状态但无法获取资源指标的"僵尸"节点，并通过飞书机器人发送告警。

## 功能特性

- 🔍 **实时监控**：持续监控集群中所有节点的 metrics 状态
- 🧟 **僵尸检测**：识别状态为 Ready 但长时间无资源指标的节点
- 📢 **飞书告警**：通过飞书机器人及时发送告警通知
- 🛡️ **智能去重**：避免重复告警，可配置告警间隔
- 🔧 **灵活配置**：支持通过环境变量自定义监控参数
- 📊 **详细日志**：提供完整的监控日志和告警记录

## 工作原理

1. 定期查询 Kubernetes API 获取所有节点状态
2. 通过 metrics-server API 获取节点资源使用情况
3. 当发现节点处于 Ready 状态但超过阈值时间没有 metrics 数据时触发告警
4. 通过飞书 Webhook 发送告警消息

## 部署要求

- Kubernetes 1.16+
- metrics-server 已部署在集群中
- 飞书机器人 Webhook URL

## 环境变量配置

| 变量名 | 说明 | 默认值 | 示例 |
|--------|------|--------|------|
| `CLUSTER_NAME` | 集群名称 | 可选 | `default` |
| `FEISHU_WEBHOOK_URL` | 飞书机器人 Webhook URL | 必填 | `https://open.feishu.cn/open-apis/bot/v2/hook/xxx` |
| `CHECK_INTERVAL` | 检查节点状态的间隔 | `30s` | `1m`, `30s` |
| `ALERT_THRESHOLD` | 无 metrics 多久后触发告警 | `1m` | `3m`, `180s` |
| `ALERT_INTERVAL` | 同一节点重复告警的最小间隔 | `5m` | `10m`, `600s` |

## 告警示例

当检测到僵尸节点时，会收到如下飞书消息：

```markdown
⚠️ 节点监控告警

节点名称: worker-node-1
问题描述: 节点状态为 Ready，但已经 5m30s 没有资源指标数据
告警时间: 2024-01-15 10:30:45
```
