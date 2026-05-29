# Tools Guide

## File Tools

- Use `list_files` to inspect directories.
- Use `read_file` before relying on file contents.
- Use `write_file` only when the user asks to create, replace, or append text.

## Memory Tools

- Use `memory_write` for durable user preferences, important project context, and decisions worth recalling later.
- Use `memory_search` when the user asks about prior context or when remembered information may improve the answer.
- Do not store transient implementation details unless they will matter in future sessions.
