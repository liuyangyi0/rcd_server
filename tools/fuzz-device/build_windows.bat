@echo off
setlocal
pushd "%~dp0"
if errorlevel 1 (
    echo [ERROR] Cannot enter script directory: %~dp0
    exit /b 1
)

echo ============================================
echo  Build fuzz_device.exe for Windows
echo ============================================

set "PYTHON_EXE="
if defined RCD_FUZZ_PYTHON (
    if exist "%RCD_FUZZ_PYTHON%" set "PYTHON_EXE=%RCD_FUZZ_PYTHON%"
)
if not defined PYTHON_EXE (
    if defined CONDA_PREFIX (
        if exist "%CONDA_PREFIX%\python.exe" set "PYTHON_EXE=%CONDA_PREFIX%\python.exe"
    )
)
if not defined PYTHON_EXE (
    if exist "%USERPROFILE%\miniconda3\python.exe" set "PYTHON_EXE=%USERPROFILE%\miniconda3\python.exe"
)
if not defined PYTHON_EXE (
    if exist "%USERPROFILE%\anaconda3\python.exe" set "PYTHON_EXE=%USERPROFILE%\anaconda3\python.exe"
)
if not defined PYTHON_EXE (
    for /f "delims=" %%I in ('where python.exe 2^>nul') do if not defined PYTHON_EXE set "PYTHON_EXE=%%I"
)

if not defined PYTHON_EXE (
    echo [ERROR] Python not found.
    echo Install Python, activate Conda, or set RCD_FUZZ_PYTHON to python.exe.
    popd
    endlocal
    pause
    exit /b 1
)

if not exist output mkdir output

echo.
echo [1/3] Installing dependencies...
"%PYTHON_EXE%" -m pip install --quiet pyserial pyinstaller
if errorlevel 1 (
    echo [ERROR] Failed to install dependencies.
    popd
    endlocal
    pause
    exit /b 1
)

echo.
echo [2/3] Running PyInstaller...
"%PYTHON_EXE%" -m PyInstaller ^
    --onefile ^
    --windowed ^
    --name fuzz_device ^
    --distpath output ^
    --workpath build ^
    --specpath build ^
    --hidden-import serial.tools.list_ports ^
    --hidden-import tkinter ^
    --hidden-import tkinter.ttk ^
    --hidden-import tkinter.scrolledtext ^
    --hidden-import tkinter.filedialog ^
    --hidden-import tkinter.messagebox ^
    --noconfirm ^
    fuzz_device.py
if errorlevel 1 (
    echo [ERROR] PyInstaller failed.
    popd
    endlocal
    pause
    exit /b 1
)

echo.
echo [3/3] Done!
echo.
echo Output: tools\fuzz-device\output\fuzz_device.exe
echo.
echo Double-click fuzz_device.exe to run.
echo.
popd
endlocal
pause
