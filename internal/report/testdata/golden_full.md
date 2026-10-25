# kintsugi report · kintsugi

// 01 summary
project          kintsugi
findings         4 lines
repairs          2 rows, 5 events
refusals         2 rows, 4 events
quota            2 provider(s)
thinking         no data

// 02 findings
## findings

- `kintsugi doctor` reports a clean environment
- session store has 4 stores, 2 legacy JSONL files

// 03 refusal telemetry
count  model                          persona               class               outcome
3      deepseek/deepseek-chat         deepseek-flash41      safety              handoff
1      qwen/qwen3-coder               _base                 policy              retried

// 04 repair telemetry
count  model                          tool            class
4      qwen/qwen3-coder               bash            arg_quotes
1      deepseek/deepseek-chat         edit            unrepaired

// 05 quota
provider                 used        limit reset    note
deepseek               250000      2000000 daily    flat plan
moonshot                12000    unlimited -        -

