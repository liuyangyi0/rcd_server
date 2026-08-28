# musl 静态编译脚本
# 用法: .\build_musl.ps1

$ContainerName = "musl_build2"
$Image = "rust:1.87-alpine"
$OutputDir = "musl_output"

Write-Host "=== 1. 清理旧容器 ===" -ForegroundColor Cyan
docker rm -f $ContainerName 2>$null

Write-Host "=== 2. 创建编译容器 ===" -ForegroundColor Cyan
docker run -d --name $ContainerName $Image sh -c "sleep 3600"

Write-Host "=== 3. 安装系统依赖 ===" -ForegroundColor Cyan
docker exec $ContainerName apk add --no-cache musl-dev pkgconfig libudev-zero-dev

Write-Host "=== 4. 拷贝源码到容器 ===" -ForegroundColor Cyan
docker exec $ContainerName mkdir -p /app
docker cp src ${ContainerName}:/app/src
docker cp serialport-rs ${ContainerName}:/app/serialport-rs
docker cp Cargo.toml ${ContainerName}:/app/Cargo.toml
docker cp Cargo.lock ${ContainerName}:/app/Cargo.lock
docker cp pki ${ContainerName}:/app/pki

Write-Host "=== 5. 编译 (release) ===" -ForegroundColor Cyan
docker exec $ContainerName sh -c "cd /app && cargo build --release --target x86_64-unknown-linux-musl"

if ($LASTEXITCODE -ne 0) {
    Write-Host "编译失败！" -ForegroundColor Red
    exit 1
}

Write-Host "=== 6. 拷贝产物到本地 ===" -ForegroundColor Cyan
if (!(Test-Path $OutputDir)) { New-Item -ItemType Directory -Path $OutputDir }
docker cp ${ContainerName}:/app/target/x86_64-unknown-linux-musl/release/rcd_server "$OutputDir/rcd_server"
if ($LASTEXITCODE -ne 0) {
    Write-Host "复制编译产物失败！" -ForegroundColor Red
    docker rm -f $ContainerName | Out-Null
    exit 1
}

Write-Host "=== 7. 清理容器 ===" -ForegroundColor Cyan
docker rm -f $ContainerName

Write-Host "=== 完成! 产物在 $OutputDir/rcd_server ===" -ForegroundColor Green
