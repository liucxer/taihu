#!/usr/bin/env python3
"""
AI模型网关稳定性测试工具（无需外部依赖）
测试指定时间内各个模型的稳定性指标
"""

import os
import urllib.request
import urllib.error
import json
import time
import threading
from collections import defaultdict
from datetime import datetime
from concurrent.futures import ThreadPoolExecutor

# 配置
GATEWAY_URL = os.environ.get("GATEWAY_URL", "http://10.155.208.190:31114/aigateway")
API_TOKEN = os.environ.get("API_TOKEN", "")
TEST_DURATION = 60  # 测试时长（秒）
TEST_MESSAGE = "Hello, please respond with 'OK' to confirm you are working."
MAX_WORKERS = 5  # 并发线程数

HEADERS = {
    "Authorization": f"Bearer {API_TOKEN}",
    "Content-Type": "application/json"
}


def get_available_models():
    """获取可用模型列表"""
    print("正在获取可用模型列表...")
    try:
        req = urllib.request.Request(
            f"{GATEWAY_URL}/v1/models",
            headers=HEADERS,
            method='GET'
        )
        with urllib.request.urlopen(req, timeout=10) as response:
            data = json.loads(response.read().decode('utf-8'))
            models = [m["id"] for m in data.get("data", [])]
            print(f"找到 {len(models)} 个模型: {', '.join(models)}\n")
            return models
    except Exception as e:
        print(f"获取模型列表时出错: {e}\n")
        return []


def test_model_stability(model_name, stop_event, request_log):
    """测试单个模型的稳定性"""
    stats = {
        "total_requests": 0,
        "successful_requests": 0,
        "failed_requests": 0,
        "total_latency": 0,
        "min_latency": float('inf'),
        "max_latency": 0,
        "latencies": [],
        "errors": defaultdict(int)
    }

    endpoint = f"{GATEWAY_URL}/v1/chat/completions"
    payload = json.dumps({
        "model": model_name,
        "messages": [{"role": "user", "content": TEST_MESSAGE}],
        "max_tokens": 50,
        "temperature": 0.7
    }).encode('utf-8')

    request_num = 0
    while not stop_event.is_set():
        request_num += 1
        stats["total_requests"] += 1
        start_time = time.time()
        timestamp = datetime.now().strftime('%Y-%m-%d %H:%M:%S')

        try:
            req = urllib.request.Request(
                endpoint,
                data=payload,
                headers=HEADERS,
                method='POST'
            )
            with urllib.request.urlopen(req, timeout=30) as response:
                response.read()
                latency = time.time() - start_time
                stats["total_latency"] += latency
                stats["latencies"].append(latency)
                stats["min_latency"] = min(stats["min_latency"], latency)
                stats["max_latency"] = max(stats["max_latency"], latency)
                stats["successful_requests"] += 1

                request_log.append({
                    "model": model_name,
                    "request_num": request_num,
                    "timestamp": timestamp,
                    "status": "success",
                    "latency": round(latency, 3),
                    "error": ""
                })

        except urllib.error.HTTPError as e:
            latency = time.time() - start_time
            stats["total_latency"] += latency
            stats["latencies"].append(latency)
            stats["min_latency"] = min(stats["min_latency"], latency)
            stats["max_latency"] = max(stats["max_latency"], latency)
            stats["failed_requests"] += 1
            error_msg = f"HTTP {e.code}"
            stats["errors"][error_msg] += 1

            request_log.append({
                "model": model_name,
                "request_num": request_num,
                "timestamp": timestamp,
                "status": "failed",
                "latency": round(latency, 3),
                "error": error_msg
            })

        except urllib.error.URLError as e:
            latency = time.time() - start_time
            stats["total_latency"] += latency
            stats["latencies"].append(latency)
            stats["min_latency"] = min(stats["min_latency"], latency)
            stats["max_latency"] = max(stats["max_latency"], latency)
            stats["failed_requests"] += 1

            error_type = "Timeout" if "timed out" in str(e.reason) else f"URL Error: {e.reason}"
            stats["errors"][error_type] += 1

            request_log.append({
                "model": model_name,
                "request_num": request_num,
                "timestamp": timestamp,
                "status": "failed",
                "latency": round(latency, 3),
                "error": error_type
            })

        except Exception as e:
            latency = time.time() - start_time
            stats["total_latency"] += latency
            stats["latencies"].append(latency)
            stats["min_latency"] = min(stats["min_latency"], latency)
            stats["max_latency"] = max(stats["max_latency"], latency)
            stats["failed_requests"] += 1
            error_type = str(type(e).__name__)
            stats["errors"][error_type] += 1

            request_log.append({
                "model": model_name,
                "request_num": request_num,
                "timestamp": timestamp,
                "status": "failed",
                "latency": round(latency, 3),
                "error": error_type
            })

        # 短暂延迟避免过于频繁的请求
        time.sleep(2)

    return model_name, stats


def print_results(results):
    """打印测试结果"""
    print("\n" + "=" * 90)
    print("测试结果汇总")
    print("=" * 90)

    # 按成功率排序
    sorted_results = sorted(
        results.items(),
        key=lambda x: x[1]["successful_requests"] / max(x[1]["total_requests"], 1),
        reverse=True
    )

    print(f"{'模型名称':<35} {'成功率':<10} {'总请求':<10} {'成功':<8} {'失败':<8} {'平均延迟':<12} {'主要错误'}")
    print("-" * 90)

    for model_name, stats in sorted_results:
        total = stats["total_requests"]
        success = stats["successful_requests"]
        failed = stats["failed_requests"]
        success_rate = (success / max(total, 1)) * 100
        avg_latency = stats["total_latency"] / max(total, 1) if total > 0 else 0

        # 获取主要错误
        main_error = "无"
        if stats["errors"]:
            main_error = max(stats["errors"].items(), key=lambda x: x[1])
            main_error = f"{main_error[0]}({main_error[1]}次)"

        print(f"{model_name:<35} {success_rate:<9.1f}% {total:<10} {success:<8} {failed:<8} {avg_latency:<11.2f}s {main_error}")

    print("\n" + "=" * 90)

    # 推荐最稳定的模型
    if sorted_results:
        best_model = sorted_results[0][0]
        best_stats = sorted_results[0][1]
        best_rate = (best_stats["successful_requests"] / max(best_stats["total_requests"], 1)) * 100
        print(f"\n✓ 推荐使用的最稳定模型: {best_model}")
        print(f"  成功率: {best_rate:.1f}%")
        print(f"  平均延迟: {best_stats['total_latency'] / max(best_stats['total_requests'], 1):.2f}s")
        print(f"  总请求数: {best_stats['total_requests']}")
        print(f"  成功请求: {best_stats['successful_requests']}")

    print("=" * 90)


def generate_markdown_report(results, request_log, test_start_time, test_end_time, all_models_count):
    """生成Markdown格式的详细测试报告"""
    
    timestamp = datetime.now().strftime('%Y-%m-%d_%H-%M-%S')
    report_file = f"model_stability_report_{timestamp}.md"

    # 计算统计数据
    sorted_results = sorted(
        results.items(),
        key=lambda x: x[1]["successful_requests"] / max(x[1]["total_requests"], 1),
        reverse=True
    )
    
    total_requests = sum(s["total_requests"] for s in results.values())
    total_success = sum(s["successful_requests"] for s in results.values())
    total_failed = sum(s["failed_requests"] for s in results.values())
    overall_success_rate = (total_success / max(total_requests, 1)) * 100
    
    # 找到最快和最慢的模型
    fastest_model = None
    fastest_latency = float('inf')
    slowest_model = None
    slowest_latency = 0
    
    for model_name, stats in results.items():
        if stats["total_requests"] > 0:
            avg_latency = stats["total_latency"] / stats["total_requests"]
            if avg_latency < fastest_latency:
                fastest_latency = avg_latency
                fastest_model = model_name
            if avg_latency > slowest_latency:
                slowest_latency = avg_latency
                slowest_model = model_name

    # 生成报告内容
    report_lines = []
    
    # 报告标题
    report_lines.append("# AI模型网关稳定性测试报告\n")
    report_lines.append(f"**生成时间**: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}\n")
    
    # 测试配置
    report_lines.append("## 一、测试配置\n")
    report_lines.append("| 配置项 | 值 |")
    report_lines.append("|--------|-----|")
    report_lines.append(f"| 网关地址 | `{GATEWAY_URL}` |")
    report_lines.append(f"| 测试时长 | {TEST_DURATION} 秒 |")
    report_lines.append(f"| 并发线程数 | {MAX_WORKERS} |")
    report_lines.append(f"| 测试消息 | `{TEST_MESSAGE}` |")
    report_lines.append(f"| 请求间隔 | 2 秒 |")
    report_lines.append(f"| 测试开始时间 | {test_start_time} |")
    report_lines.append(f"| 测试结束时间 | {test_end_time} |")
    report_lines.append(f"| 可用模型总数 | {all_models_count} 个 |")
    report_lines.append(f"| 实际测试模型数 | {len(results)} 个 |\n")
    
    # 总体统计
    report_lines.append("## 二、总体统计\n")
    report_lines.append("| 指标 | 值 |")
    report_lines.append("|------|-----|")
    report_lines.append(f"| 总请求数 | {total_requests} |")
    report_lines.append(f"| 成功请求数 | {total_success} |")
    report_lines.append(f"| 失败请求数 | {total_failed} |")
    report_lines.append(f"| 总体成功率 | {overall_success_rate:.1f}% |")
    report_lines.append(f"| 最快模型 | {fastest_model} ({fastest_latency:.2f}s) |")
    report_lines.append(f"| 最慢模型 | {slowest_model} ({slowest_latency:.2f}s) |\n")
    
    # 各模型详细测试结果
    report_lines.append("## 三、各模型详细测试结果\n")
    report_lines.append("### 3.1 性能指标汇总\n")
    report_lines.append("| 排名 | 模型名称 | 成功率 | 总请求 | 成功 | 失败 | 平均延迟 | 最小延迟 | 最大延迟 | 延迟标准差 |")
    report_lines.append("|------|----------|--------|--------|------|------|----------|----------|----------|------------|")
    
    for rank, (model_name, stats) in enumerate(sorted_results, 1):
        total = stats["total_requests"]
        success = stats["successful_requests"]
        failed = stats["failed_requests"]
        success_rate = (success / max(total, 1)) * 100
        
        if total > 0:
            avg_latency = stats["total_latency"] / total
            min_latency = stats["min_latency"] if stats["min_latency"] != float('inf') else 0
            max_latency = stats["max_latency"]
            
            # 计算标准差
            if len(stats["latencies"]) > 1:
                mean = avg_latency
                variance = sum((x - mean) ** 2 for x in stats["latencies"]) / len(stats["latencies"])
                std_dev = variance ** 0.5
            else:
                std_dev = 0
        else:
            avg_latency = min_latency = max_latency = std_dev = 0
        
        report_lines.append(
            f"| {rank} | `{model_name}` | {success_rate:.1f}% | {total} | {success} | {failed} | "
            f"{avg_latency:.2f}s | {min_latency:.2f}s | {max_latency:.2f}s | {std_dev:.2f}s |"
        )
    
    report_lines.append("")
    
    # 详细请求日志
    report_lines.append("### 3.2 详细请求日志\n")
    
    # 按模型分组显示日志
    models_in_log = set(log["model"] for log in request_log)
    
    for model_name in [m for m, _ in sorted_results]:
        model_logs = [log for log in request_log if log["model"] == model_name]
        if not model_logs:
            continue
        
        report_lines.append(f"#### {model_name}\n")
        report_lines.append(f"- **总请求数**: {len(model_logs)}")
        success_count = sum(1 for log in model_logs if log["status"] == "success")
        failed_count = sum(1 for log in model_logs if log["status"] == "failed")
        report_lines.append(f"- **成功**: {success_count} | **失败**: {failed_count}\n")
        
        report_lines.append("| 序号 | 时间戳 | 状态 | 延迟 | 错误信息 |")
        report_lines.append("|------|--------|------|------|----------|")
        
        for log in model_logs:
            status_icon = "✅" if log["status"] == "success" else "❌"
            error_info = log["error"] if log["error"] else "-"
            report_lines.append(
                f"| {log['request_num']} | {log['timestamp']} | {status_icon} {log['status']} | "
                f"{log['latency']}s | {error_info} |"
            )
        
        report_lines.append("")
    
    # 错误分析
    report_lines.append("## 四、错误分析\n")
    
    all_errors = defaultdict(int)
    for _, stats in results.items():
        for error, count in stats["errors"].items():
            all_errors[error] += count
    
    if all_errors:
        report_lines.append("| 错误类型 | 出现次数 | 影响模型 |")
        report_lines.append("|----------|----------|----------|")
        
        for error, count in sorted(all_errors.items(), key=lambda x: x[1], reverse=True):
            # 找出受影响的模型
            affected_models = []
            for model_name, stats in results.items():
                if error in stats["errors"]:
                    affected_models.append(model_name)
            
            report_lines.append(f"| {error} | {count} | {', '.join(f'`{m}`' for m in affected_models)} |")
    else:
        report_lines.append("✅ 测试期间未发生任何错误。\n")
    
    report_lines.append("")
    
    # 稳定性评级
    report_lines.append("## 五、稳定性评级\n")
    report_lines.append("根据成功率和请求响应情况，对各个模型进行评级：\n")
    report_lines.append("| 模型名称 | 成功率 | 平均延迟 | 评级 | 说明 |")
    report_lines.append("|----------|--------|----------|------|------|")
    
    for model_name, stats in sorted_results:
        total = stats["total_requests"]
        success = stats["successful_requests"]
        success_rate = (success / max(total, 1)) * 100
        avg_latency = stats["total_latency"] / max(total, 1) if total > 0 else 0
        
        if success_rate == 100 and total > 20:
            rating = "⭐⭐⭐⭐⭐"
            desc = "极佳 - 完全稳定"
        elif success_rate >= 90 and total > 15:
            rating = "⭐⭐⭐⭐"
            desc = "优秀 - 高度稳定"
        elif success_rate >= 80:
            rating = "⭐⭐⭐"
            desc = "良好 - 基本稳定"
        elif success_rate >= 50:
            rating = "⭐⭐"
            desc = "一般 - 存在不稳定因素"
        elif total == 0:
            rating = "❓"
            desc = "无响应 - 可能已下线"
        else:
            rating = "⭐"
            desc = "较差 - 不推荐使用"
        
        report_lines.append(f"| `{model_name}` | {success_rate:.1f}% | {avg_latency:.2f}s | {rating} | {desc} |")
    
    report_lines.append("")
    
    # 推荐建议
    report_lines.append("## 六、推荐与建议\n")
    
    if sorted_results:
        best_model = sorted_results[0][0]
        best_stats = sorted_results[0][1]
        best_rate = (best_stats["successful_requests"] / max(best_stats["total_requests"], 1)) * 100
        best_avg_latency = best_stats["total_latency"] / max(best_stats["total_requests"], 1)
        
        report_lines.append("### 🏆 最佳推荐\n")
        report_lines.append(f"**最稳定模型**: `{best_model}`\n")
        report_lines.append(f"- 成功率: {best_rate:.1f}%")
        report_lines.append(f"- 平均延迟: {best_avg_latency:.2f}s")
        report_lines.append(f"- 总请求数: {best_stats['total_requests']}")
        report_lines.append(f"- 成功请求: {best_stats['successful_requests']}\n")
        
        # 找出前3名
        report_lines.append("### 推荐排名 Top 3\n")
        report_lines.append("| 排名 | 模型 | 成功率 | 平均延迟 | 适用场景 |")
        report_lines.append("|------|------|--------|----------|----------|")
        
        for i, (model_name, stats) in enumerate(sorted_results[:3], 1):
            if stats["total_requests"] == 0:
                continue
            rate = (stats["successful_requests"] / stats["total_requests"]) * 100
            avg_lat = stats["total_latency"] / stats["total_requests"]
            
            if i == 1:
                scenario = "生产环境首选"
            elif i == 2:
                scenario = "备用模型"
            else:
                scenario = "降级方案"
            
            report_lines.append(f"| {i} | `{model_name}` | {rate:.1f}% | {avg_lat:.2f}s | {scenario} |")
    
    report_lines.append("")
    report_lines.append("---\n")
    report_lines.append("*本报告由 AI模型网关稳定性测试工具 自动生成*")
    
    # 写入文件
    with open(report_file, 'w', encoding='utf-8') as f:
        f.write('\n'.join(report_lines))
    
    print(f"\n📄 详细测试报告已生成: {report_file}")
    return report_file


def main():
    print("=" * 90)
    print("AI模型网关稳定性测试工具")
    print("=" * 90)
    print(f"网关地址: {GATEWAY_URL}")
    print(f"测试时长: {TEST_DURATION} 秒")
    
    test_start_time = datetime.now().strftime('%Y-%m-%d %H:%M:%S')
    print(f"开始时间: {test_start_time}")
    print()

    # 获取可用模型
    models = get_available_models()
    all_models_count = len(models)

    if not models:
        print("未找到可用模型，测试终止。")
        return

    # 限制测试模型数量（避免过多模型同时测试）
    if len(models) > 10:
        print(f"模型数量较多({len(models)}个)，将测试前10个模型...\n")
        models = models[:10]

    # 开始测试
    print(f"开始测试 {len(models)} 个模型，持续 {TEST_DURATION} 秒...")
    print("每个模型每2秒发送一次请求，测试其稳定性...")
    print("=" * 90 + "\n")

    stop_event = threading.Event()
    results = {}
    request_log = []  # 记录所有请求详情

    def run_test(model):
        _, stats = test_model_stability(model, stop_event, request_log)
        results[model] = stats

    # 使用线程池并发测试所有模型
    with ThreadPoolExecutor(max_workers=min(MAX_WORKERS, len(models))) as executor:
        futures = {executor.submit(run_test, model): model for model in models}

        # 等待测试结束
        time.sleep(TEST_DURATION)
        stop_event.set()

        # 等待所有线程完成
        executor.shutdown(wait=True)

    test_end_time = datetime.now().strftime('%Y-%m-%d %H:%M:%S')
    
    # 打印结果
    print_results(results)
    
    # 生成Markdown报告
    generate_markdown_report(results, request_log, test_start_time, test_end_time, all_models_count)


if __name__ == "__main__":
    main()
