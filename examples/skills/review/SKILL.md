---
name: review
description: Review a file for concrete correctness and security issues, and count its lines and characters. Use when the user asks to review or check a file.
---

# Review a supplied file

Review the file requested by the user. If its path is missing, ask for it.
Use read_file only when that tool is available; otherwise ask for the contents.

Focus on concrete correctness and security issues. For each finding, explain
the triggering input or condition, the consequence, and a small suggested fix.
Distinguish confirmed findings from assumptions. If no issue is established,
say so and identify any verification you could not perform.

Do not claim to execute tests or inspect files unless you actually did so.

For simple text metrics, you may run the installed `scripts/stats.py` through
`run_skill_script`, supplying the text as stdin. It returns JSON with line and
character counts. These metrics are not correctness or security checks.

This package does not grant workspace modifications, host shell access, or
network access. Its installed script runs within the runtime's sandbox grants.
