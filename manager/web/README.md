# RCD OPC 管理中心 Web

React + TypeScript + Vite 管理端。生产构建输出到 `dist/`，供 Go 服务使用 `go:embed` 打包进单一 EXE。

```powershell
npm install
npm run test
npm run build
```

`embed.go` 导出 `FS()` 和 `Handler()`，静态资源不依赖 Vite 开发服务器。仓库中的 `dist/embed-placeholder.txt` 仅用于保证 clean checkout 时 Go 的 embed 模式可编译；生成或发布管理程序前必须执行 `npm run build`，否则没有可用的 `index.html`。

开发服务器默认把 `/api` 代理至 `http://127.0.0.1:8080`。应用默认只访问真实同源 API；后端不可用时显示明确错误与空态，不会自动伪造成功数据。

如需离线视觉演示，可显式设置 `VITE_API_MODE=mock`。该模式会在页面底部标注“演示数据”，不得用于生产构建。
