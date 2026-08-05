#!/usr/bin/env bash

# 正式 release asset 只包含这组经过审查的 Tool。resources/tools/ 中的其他
# 本地草稿不会意外进入公开安装包；local source workflow 仍读取全部 *.json。
SN_CLI_RELEASE_TOOL_FILES=(
  web_fetch.json
  web_search.json
)
