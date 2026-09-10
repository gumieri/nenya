Output docs/demo.gif
Set FontSize 20
Set Width 1200
Set Height 640
Set Theme Dracula
Set TypingSpeed 25ms

Hide
Type "./examples/demo/up.sh"
Enter
Sleep 4s
Show

Type "export NK=nk-demo-demo-demo-demo REQ=examples/demo/request.json"
Enter
Sleep 600ms

Type "curl -s --max-time 5 http://127.0.0.1:8080/healthz"
Enter
Sleep 1s

Type "jq -r '.messages[0].content' $REQ"
Enter
Sleep 1.6s

Type "curl -sN --max-time 10 http://127.0.0.1:8080/v1/chat/completions --oauth2-bearer $NK -d @$REQ"
Enter
Sleep 2.5s

Type "jq -r '.messages[0].content' /tmp/nenya-demo/received.json"
Enter
Sleep 1.6s

Type "diff <(jq -r '.messages[0].content' $REQ) <(jq -r '.messages[0].content' /tmp/nenya-demo/received.json)"
Enter
Sleep 2s

Type "curl -s --max-time 5 http://127.0.0.1:8080/metrics | grep pipeline_redactions"
Enter
Sleep 1.2s

Hide
Type "./examples/demo/down.sh"
Enter
Sleep 500ms
