#!/usr/bin/env bash

# 正式 release asset 只包含这组经过审查的 Profile。本地 source workflow
# 仍有意读取全部 configs/*.json。
SN_CLI_RELEASE_PROFILE_FILES=(
  api-cc.json
  api-cx.json
  cc-bai.json
  cc.json
  commit.json
  cx-adv.json
  cx-deep.json
  cx-image.json
  cx-remote.json
  cx-spark.json
  cx.json
)
