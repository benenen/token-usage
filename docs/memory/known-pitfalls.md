# Known pitfalls

> 非显然的性能 / 正确性坑，按「日期 现象 → 原因 → 做法」一行一条。

- 2026-10-23 线上 tokenusage 库 model_prices（1.5 万行）被全表扫 1121 万次、usage_detail 被全表扫 1.3 万次，二级索引 0 次使用 → ① 价格 LATERAL 用 `model LIKE mp.model_prefix || '%'`，模式在表侧，btree 用不上，每个 usage_daily 行扫一遍价格表；② `RebuildDaily` 按 `(ts AT TIME ZONE 'UTC')::date` 过滤，表达式包住列，`idx_detail_ts` 用不上，每 10 分钟全表扫一次 → 价格查找统一走 `store.go` 的 `priceLookupSQL`（枚举 model 的全部前缀后用 `= ANY` 等值匹配，走 `idx_model_prices_range`），按时间过滤一律写成 `ts >= $from AND ts < $to` 的裸列范围。临时文件 22GB 的来源未经 pg_stat_statements 确认（待验证）。
