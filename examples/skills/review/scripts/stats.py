import json
import sys

text = sys.stdin.read()
print(json.dumps({"lines": len(text.splitlines()), "characters": len(text)}))
