#!/usr/bin/env bash
#
# verify-ci.sh - Wait for and verify GitHub Actions workflows pass
#
# Usage:
#   verify-ci.sh --branch BRANCH_NAME [--timeout MINUTES]
#   verify-ci.sh --run-id RUN_ID [--timeout MINUTES]
#
# Exit codes:
#   0 - All workflows passed
#   1 - One or more workflows failed or timeout reached

set -euo pipefail

# Default values
TIMEOUT_MINUTES=10
BRANCH=""
RUN_ID=""
MODE=""

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Parse command line arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    --branch)
      BRANCH="$2"
      MODE="branch"
      shift 2
      ;;
    --run-id)
      RUN_ID="$2"
      MODE="run-id"
      shift 2
      ;;
    --timeout)
      TIMEOUT_MINUTES="$2"
      shift 2
      ;;
    -h|--help)
      echo "Usage: $0 --branch BRANCH_NAME [--timeout MINUTES]"
      echo "       $0 --run-id RUN_ID [--timeout MINUTES]"
      echo ""
      echo "Options:"
      echo "  --branch BRANCH    Verify workflows for a specific branch"
      echo "  --run-id RUN_ID    Verify a specific workflow run"
      echo "  --timeout MINUTES  Timeout in minutes (default: 10)"
      echo "  -h, --help         Show this help message"
      exit 0
      ;;
    *)
      echo "Unknown option: $1"
      exit 1
      ;;
  esac
done

# Validate arguments
if [[ -z "$MODE" ]]; then
  echo -e "${RED}Error: Must specify either --branch or --run-id${NC}" >&2
  echo "Use -h or --help for usage information" >&2
  exit 1
fi

# Check if gh CLI is installed
if ! command -v gh &> /dev/null; then
  echo -e "${RED}Error: gh CLI is not installed${NC}" >&2
  echo "Install it from: https://cli.github.com/" >&2
  exit 1
fi

# Function to wait for a specific run to complete
wait_for_run() {
  local run_id=$1
  local timeout_seconds=$((TIMEOUT_MINUTES * 60))
  local start_time=$(date +%s)

  echo -e "${BLUE}Waiting for workflow run ${run_id} to complete (timeout: ${TIMEOUT_MINUTES}m)...${NC}"

  while true; do
    # Get run status
    local status=$(gh run view "$run_id" --json status --jq '.status' 2>/dev/null || echo "unknown")

    if [[ "$status" == "completed" ]]; then
      break
    fi

    # Check timeout
    local current_time=$(date +%s)
    local elapsed=$((current_time - start_time))
    if [[ $elapsed -ge $timeout_seconds ]]; then
      echo -e "${RED}Timeout: Workflow did not complete within ${TIMEOUT_MINUTES} minutes${NC}" >&2
      return 1
    fi

    # Wait before checking again
    sleep 5
  done

  return 0
}

# Function to check run conclusion
check_run_conclusion() {
  local run_id=$1

  # Get run details
  local run_info=$(gh run view "$run_id" --json conclusion,status,workflowName,headBranch,displayTitle)
  local conclusion=$(echo "$run_info" | jq -r '.conclusion')
  local workflow_name=$(echo "$run_info" | jq -r '.workflowName')
  local branch=$(echo "$run_info" | jq -r '.headBranch')
  local title=$(echo "$run_info" | jq -r '.displayTitle')

  echo ""
  echo -e "${BLUE}Workflow:${NC} $workflow_name"
  echo -e "${BLUE}Branch:${NC} $branch"
  echo -e "${BLUE}Commit:${NC} $title"

  if [[ "$conclusion" == "success" ]]; then
    echo -e "${GREEN}✓ Status: PASSED${NC}"
    return 0
  else
    echo -e "${RED}✗ Status: FAILED (conclusion: $conclusion)${NC}"

    # Show job details
    echo ""
    echo "Job details:"
    gh run view "$run_id" --json jobs --jq '.jobs[] | "  \(.name): \(.conclusion)"'

    return 1
  fi
}

# Function to get latest runs for a branch
get_branch_runs() {
  local branch=$1

  # Get the latest workflow runs for this branch
  # We look for the most recent runs (limit 10) and filter by branch
  gh run list --branch "$branch" --limit 10 --json databaseId,status,workflowName,createdAt \
    --jq 'group_by(.workflowName) | map(.[0]) | map(.databaseId) | .[]'
}

# Main logic
case "$MODE" in
  run-id)
    echo -e "${BLUE}=== CI Verification (Run ID Mode) ===${NC}"

    if ! wait_for_run "$RUN_ID"; then
      exit 1
    fi

    if check_run_conclusion "$RUN_ID"; then
      echo ""
      echo -e "${GREEN}✓ CI verification passed${NC}"
      exit 0
    else
      echo ""
      echo -e "${RED}✗ CI verification failed${NC}"
      exit 1
    fi
    ;;

  branch)
    echo -e "${BLUE}=== CI Verification (Branch Mode) ===${NC}"
    echo -e "${BLUE}Branch:${NC} $BRANCH"
    echo ""

    # Get latest runs for each workflow on this branch
    echo "Fetching workflow runs for branch '$BRANCH'..."
    RUN_IDS=$(get_branch_runs "$BRANCH")

    if [[ -z "$RUN_IDS" ]]; then
      echo -e "${YELLOW}Warning: No workflow runs found for branch '$BRANCH'${NC}" >&2
      exit 1
    fi

    echo "Found $(echo "$RUN_IDS" | wc -l | tr -d ' ') workflow run(s)"
    echo ""

    # Wait for and check each run
    ALL_PASSED=true
    for run_id in $RUN_IDS; do
      echo -e "${BLUE}--- Checking run $run_id ---${NC}"

      if ! wait_for_run "$run_id"; then
        ALL_PASSED=false
        continue
      fi

      if ! check_run_conclusion "$run_id"; then
        ALL_PASSED=false
      fi

      echo ""
    done

    # Final summary
    echo -e "${BLUE}=== Summary ===${NC}"
    if [[ "$ALL_PASSED" == "true" ]]; then
      echo -e "${GREEN}✓ All workflows passed${NC}"
      exit 0
    else
      echo -e "${RED}✗ One or more workflows failed${NC}"
      exit 1
    fi
    ;;
esac
