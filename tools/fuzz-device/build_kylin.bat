@echo off
cd /d "%~dp0"
if errorlevel 1 exit /b 1
echo ============================================
echo  Build fuzz_device for Kylin Linux x86_64
echo ============================================

docker version >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Docker not found or not running.
    echo Please start Docker Desktop first.
    pause
    exit /b 1
)

if not exist output mkdir output

echo.
echo [1/3] Building Docker image...
docker build -f Dockerfile.build -t fuzz-device-builder .
if errorlevel 1 (
    echo [ERROR] Docker image build failed.
    pause
    exit /b 1
)

echo.
echo [2/3] Running PyInstaller...
docker run --rm -v "%CD%\output:/output" fuzz-device-builder
if errorlevel 1 (
    echo [ERROR] PyInstaller failed.
    pause
    exit /b 1
)

echo.
echo [3/3] Done!
echo.
echo Output: tools\fuzz-device\output\fuzz_device
echo.
echo On Kylin machine:
echo   chmod +x fuzz_device
echo   ./fuzz_device
echo.
pause
