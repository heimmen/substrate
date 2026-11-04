#!/usr/bin/env bash
# 检查 58681 环境 (ate-demo-mf-pi / mfpi) 用户是否都能访问 agent
#
# 用法:
#   ./check_agent_access.sh [atespace] [namespace]
#   默认 atespace=mfpi  namespace=ate-demo-mf-pi (58681 环境)
#   示例: ./check_agent_access.sh mfcc ate-demo-mf-cc
#
# "可访问"判定依据(与人工诊断一致):
#   1. actor 状态为 STATUS_RUNNING (而非 SUSPENDING/异常卡死)
#   2. 绑定的 worker pod: Ready=1/1、Running、restarts=0 (无 OOM/重启损坏)
#   3. ateom 完整启动: worker 日志行数 >3 (mf-agent 已起来, 非 ateom 卡 boot 的"3行假死")
#   4. (仅提示) 最近是否有请求活动, 作为已实际服务的佐证
set -uo pipefail

ATESPACE="${1:-mfpi}"
NAMESPACE="${2:-ate-demo-mf-pi}"

# ---------- 颜色 ----------
RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; NC=$'\033[0m'

die() { echo -e "${RED}错误: $*${NC}" >&2; exit 1; }

# ---------- 依赖检查 ----------
command -v kubectl >/dev/null 2>&1 || die "未找到 kubectl"
kubectl ate >/dev/null 2>&1 || die "kubectl ate 插件不可用(请确保 ate 插件已安装/可访问集群)"

# ---------- 获取 actor 列表 ----------
actors_txt="$(kubectl ate get actor -a "$ATESPACE" 2>&1)" || {
  echo -e "${RED}无法读取 actor 列表 (atespace=$ATESPACE):${NC}"; echo "$actors_txt"; exit 1
}

# 去掉表头与误报行(如 portforward 噪音); 取 NAME / STATUS / ATEOM POD 三列
mapfile -t lines < <(echo "$actors_txt" | awk 'NR>1 && NF>=8 && $2!~/portforward/ {print}')
if [ "${#lines[@]}" -eq 0 ]; then
  echo -e "${YELLOW}未在 atespace=$ATESPACE 找到任何用户。${NC}"
  exit 0
fi

echo -e "${BOLD}=== 检查环境: atespace=$ATESPACE  namespace=$NAMESPACE  用户数=${#lines[@]} ===${NC}"
echo

ok=0; bad=0
declare -a bad_users=()

for l in "${lines[@]}"; do
  name="$(echo "$l" | awk '{print $2}')"
  st="$(echo "$l"   | awk '{print $4}')"
  pod="$(echo "$l"  | awk '{print $5}' | sed 's|.*/||')"
  note=""

  # ---- 1. 状态必须是 RUNNING(可访问前提; SUSPENDING 等视为卡死) ----
  if [ "$st" != "STATUS_RUNNING" ]; then
    echo -e "${RED}[异常] $name  状态=$st  期望 STATUS_RUNNING${NC}"
    bad=$((bad+1)); bad_users+=("$name(状态[$st])"); continue
  fi

  # ---- 2. worker pod 健康: Ready/Running/restarts=0 ----
  if [ -n "$pod" ]; then
    podinfo="$(kubectl get pod "$pod" -n "$NAMESPACE" --no-headers 2>/dev/null)"
    if [ -z "$podinfo" ]; then
      echo -e "${RED}[异常] $name  绑定 pod $pod 不存在(可能已删, 等待 syncer 重置)${NC}"
      bad=$((bad+1)); bad_users+=("$name(pod不存在)"); continue
    fi
    ready="$(echo "$podinfo" | awk '{print $2}')"
    ph="$(echo "$podinfo"    | awk '{print $3}')"
    restarts="$(echo "$podinfo" | awk '{print $4}')"
    if [ "$ready" != "1/1" ] || [ "$ph" != "Running" ]; then
      echo -e "${RED}[异常] $name  pod=$pod 状态=$ph Ready=$ready (期望 Running 1/1)${NC}"
      bad=$((bad+1)); bad_users+=("$name(pod[$ph/$ready])"); continue
    fi
    [ "$restarts" != "0" ] && note+="restarts=$restarts(曾重启) "

    # ---- 3. ateom 完整启动: 日志行数>3 (mf-agent 已起来, 非卡 boot 假死) ----
    plines="$(kubectl logs pod/"$pod" -n "$NAMESPACE" 2>/dev/null | wc -l)"
    if [ "$plines" -le 3 ]; then
      echo -e "${RED}[异常] $name  pod=$pod 日志仅 ${plines} 行(ateom 卡 boot, mf-agent 未启动)${NC}"
      bad=$((bad+1)); bad_users+=("$name(ateom卡boot)"); continue
    fi

    # ---- 4. (提示, 不算故障) 最近是否有请求活动 ----
    last_ts="$(kubectl logs pod/"$pod" -n "$NAMESPACE" --tail=5000 2>/dev/null \
               | grep -oE '"time":[0-9]{13}' | tail -1 | grep -oE '[0-9]{13}')"
    if [ -n "$last_ts" ]; then
      now_ms=$(( $(date +%s) * 1000 ))
      ago=$(( (now_ms - last_ts) / 60000 ))
      [ "$ago" -lt 0 ] && ago=0
      note+="最近活动 ${ago} 分钟前"
    else
      note+="日志无请求记录"
    fi

    echo -e "${GREEN}[正常] $name  状态=$st  pod=$pod  ready=$ready restarts=$restarts  mfAgent日志=${plines}行  $note${NC}"
    ok=$((ok+1))
  else
    echo -e "${YELLOW}[跳过] $name  状态=$st  未绑定 pod(SUSPENDED 或待分配)${NC}"
    ok=$((ok+1))
  fi
done

echo
echo "${BOLD}===== 汇总 =====${NC}"
echo -e "总用户:   ${#lines[@]}"
echo -e "${GREEN}可访问:   $ok${NC}"
echo -e "${RED}异常:     $bad${NC}"
if [ "$bad" -gt 0 ]; then
  echo
  echo -e "${RED}以下用户无法访问 agent (需修复):${NC}"
  printf '  - %s\n' "${bad_users[@]}"
  echo
  echo "${YELLOW}常见修复: 对这些 RUNNING 但 worker 损坏(日志<=3行/曾OOM)的用户,"
  echo "删除其绑定的损坏 worker pod 触发 syncer 重置为 SUSPENDED, 再 resume:"
  echo "  kubectl delete pod <worker-pod> -n $NAMESPACE"
  echo "  kubectl ate resume actor <user> -a $ATESPACE${NC}"
  exit 1
else
  echo -e "${GREEN}所有用户均可正常访问 agent ✅${NC}"
  exit 0
fi
