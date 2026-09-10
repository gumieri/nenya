Output docs/demo.gif
Set FontSize 22
Set Width 1080
Set Height 600
Set Theme Dracula
Set TypingSpeed 10ms

Hide
Type "./examples/demo/up.sh"
Enter
Sleep 4s
Type "clear"
Enter
Sleep 300ms
Show

Type "echo '── 1 · your request (contains fake secrets) ──'"
Enter
Sleep 400ms
Type "jq -r '.messages[].content' examples/demo/request.json"
Enter
Sleep 2s

Type "echo '── 2 · sent through nenya ──'"
Enter
Sleep 400ms
Type "curl -sN --max-time 10 http://127.0.0.1:8080/v1/chat/completions -H 'Authorization: Bearer nk-demo-demo-demo-demo' -d @examples/demo/request.json"
Enter
Sleep 1.5s

Type "echo '── 3 · what the provider actually received ──'"
Enter
Sleep 400ms
Type "jq -r '.messages[].content' /tmp/nenya-demo/received.json"
Enter
Sleep 4s

Type "echo '── proof ──'"
Enter
Sleep 400ms
Type "curl -s --max-time 5 http://127.0.0.1:8080/metrics | grep pipeline_redactions"
Enter
Sleep 1.5s

Hide
Type "./examples/demo/down.sh"
Enter
Sleep 500ms
