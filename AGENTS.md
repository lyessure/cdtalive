# 项目工作流

## 运行版本与部署

- 本项目的 Go 版本由 systemd 服务 `cdtalive.service` 运行，执行文件为项目根目录的 `./cdtalive`。
- 修改 Go 代码后，使用以下命令构建并重启服务：

  ```bash
  go build -o cdtalive ./cmd/cdtalive
  systemctl restart cdtalive
  ```

- 重启后应使用 `systemctl is-active cdtalive` 确认服务正常；需要时可请求 `http://127.0.0.1:5201/api/dashboard` 验证接口。
- Python 版本修改代码后无需在此项目中执行构建或重启操作。
- 不要使用 Docker 或 `docker compose` 构建、启动或重启 `cdtalive`。
