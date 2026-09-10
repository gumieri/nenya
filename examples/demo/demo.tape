Output docs/demo.gif
Set FontSize 22
Set Width 960
Set Height 540
Set Theme Dracula
Set TypingSpeed 10ms

Hide
Type "./examples/demo/up.sh"
Enter
Sleep 4s
Type "cd examples/demo"
Enter
Type "export GREP_COLORS='ms=01;31'"
Enter
Type "clear"
Enter
Sleep 300ms
Show

Type@1ms "printf '\n\033[1;44m 1 · YOUR REQUEST \033[0m  fake secrets inside — nothing has left your machine\n'"
Enter
Sleep 200ms
Type "jq -r '.messages[].content' request.json | grep --color=always -E 'AKIA[A-Z0-9]+|ghp_[A-Za-z0-9]+|DB_PASSWORD=.*'"
Enter
Sleep 2.2s

Type@1ms "clear"
Enter
Sleep 150ms
Type@1ms "printf '\n\033[1;46m 2 · THROUGH NENYA \033[0m  streaming reply\n'"
Enter
Sleep 200ms
Type "curl -sN --max-time 10 http://127.0.0.1:8080/v1/chat/completions -H 'Authorization: Bearer nk-demo-demo-demo-demo' -d @request.json | grep --color=always -o --line-buffered 'secrets never reached this upstream'"
Enter
Sleep 2s

Type@1ms "clear"
Enter
Sleep 150ms
Type@1ms "printf '\n\033[1;42m 3 · WHAT THE PROVIDER RECEIVED \033[0m  secrets stripped\n'"
Enter
Sleep 200ms
Type "jq -r '.messages[].content' /tmp/nenya-demo/received.json | GREP_COLORS='ms=01;32' grep --color=always -F '[REDACTED]'"
Enter
Sleep 4s

Type@1ms "clear"
Enter
Sleep 150ms
Type@1ms "printf '\n\033[1;43m 4 · PROOF \033[0m\n'"
Enter
Sleep 200ms
Type "curl -s --max-time 5 http://127.0.0.1:8080/metrics | GREP_COLORS='ms=01;33' grep --color=always -E 'nenya_pipeline_redactions_total [0-9]+'"
Enter
Sleep 2s

Hide
Type "curl -s --max-time 10 -o /dev/null http://127.0.0.1:8080/v1/chat/completions -H @/tmp/nenya-demo/auth -d @request.json"
Enter
Sleep 1.5s
Type "clear"
Enter
Sleep 200ms
Show

Type@1ms "printf '\n\033[1;43m 5 · EFFICIENCY \033[0m  same request again → cache hit; client vs upstream tokens\n'"
Enter
Sleep 200ms
Type "curl -s --max-time 5 http://127.0.0.1:8080/metrics | GREP_COLORS='ms=01;33' grep --color=always -E '^nenya_(tokens_estimated_total|pipeline_tokens_saved_total|cache_hit_total)'"
Enter
Sleep 3s

Type@1ms "printf '\n\033[1;32m✔ redacted · trimmed · cached — the upstream saw the minimum\033[0m\n\n'"
Enter
Sleep 2s

Hide
Type "./examples/demo/down.sh"
Enter
Sleep 500ms
