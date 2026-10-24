@REM build_musl.bat

@echo off
chcp 65001 >nul
echo === 清理旧容器 ===
docker rm -f musl_build2 2>nul

echo === 创建编译容器 ===
docker run -d --name musl_build2 rust:1.87-alpine sh -c "sleep 3600"
if %errorlevel% neq 0 (
    echo 创建容器失败！
    pause
    exit /b 1
)

echo === 安装系统依赖 ===
docker exec musl_build2 apk add --no-cache musl-dev pkgconfig libudev-zero-dev
if %errorlevel% neq 0 (
    echo 安装依赖失败！
    pause
    exit /b 1
)

echo === 复制源码到容器 ===
docker exec musl_build2 mkdir -p /app
docker cp src musl_build2:/app/
docker cp serialport-rs musl_build2:/app/serialport-rs
docker cp Cargo.toml musl_build2:/app/Cargo.toml
docker cp Cargo.lock musl_build2:/app/Cargo.lock
docker cp pki musl_build2:/app/

echo === 添加 musl 编译目标 ===
docker exec musl_build2 rustup target add x86_64-unknown-linux-musl

echo === 开始编译 (耗时约3分钟) ===
docker exec -w /app musl_build2 cargo build --release --target x86_64-unknown-linux-musl
if %errorlevel% neq 0 (
    echo 编译失败！
    pause
    exit /b 1
)

echo === 复制编译产物 ===
if not exist musl_output mkdir musl_output
docker cp musl_build2:/app/target/x86_64-unknown-linux-musl/release/rcd_server musl_output/rcd_server
if %errorlevel% neq 0 (
    echo 复制产物失败！
    pause
    exit /b 1
)

echo === 清理容器 ===
docker rm -f musl_build2

echo === 编译完成！输出文件: musl_output/rcd_server ===
pause
