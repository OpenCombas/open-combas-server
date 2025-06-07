@echo off
setlocal enabledelayedexpansion

REM Open Combas Benchmark Runner (Windows)
REM Simple wrapper to build and run the Go benchmark tool

echo.
echo =======================================
echo   Open Combas Benchmark Suite
echo =======================================
echo.

REM Get parameters
set MODE=%1
set OUTPUT_FILE=%2

REM Set defaults
if "%MODE%"=="" set MODE=standard
if "%OUTPUT_FILE%"=="" set OUTPUT_FILE=

echo Mode: %MODE%
if not "%OUTPUT_FILE%"=="" echo Output: %OUTPUT_FILE%
echo.

REM Check if Go is available
go version >nul 2>&1
if errorlevel 1 (
    echo ❌ Go is not installed or not in PATH
    pause
    exit /b 1
)

echo ✅ Go is available

REM Build the benchmark runner
echo.
echo 🔨 Building benchmark runner...
cd cmd\benchmark-runner
go build -o ..\..\benchmark-runner.exe .
if errorlevel 1 (
    echo ❌ Failed to build benchmark runner
    cd ..\..
    pause
    exit /b 1
)
cd ..\..

echo ✅ Benchmark runner built successfully

REM Run the benchmark
echo.
echo 🚀 Running benchmarks...
if "%OUTPUT_FILE%"=="" (
    benchmark-runner.exe -mode=%MODE%
) else (
    benchmark-runner.exe -mode=%MODE% -output=%OUTPUT_FILE%
)

set BENCHMARK_EXIT=%errorlevel%

REM Cleanup
if exist benchmark-runner.exe del benchmark-runner.exe

if %BENCHMARK_EXIT%==0 (
    echo.
    echo ✅ Benchmark completed successfully!
) else (
    echo.
    echo ❌ Benchmark failed
)

echo.
pause
exit /b %BENCHMARK_EXIT%