@echo off
chcp 65001 >nul
cd /d "%~dp0"
title LLM YONLENDIRICI (Zen Kostum + Proxy Havuzu + EVREN Kota)

echo ====================================================================
echo        🚀 LLM YÖNLENDİRİCİ — TÜM SERVİSLER TEK BİR PAKETTE
echo ====================================================================
echo.

tasklist /fi "imagename eq llm-yonlendirici.exe" | findstr /i "llm-yonlendirici.exe" >nul
if %errorlevel% equ 0 (
    echo [BILGI] LLM Yonlendirici su anda calisiyor.
    echo.
    echo  [1] Tarayiciyi Ac (Mevcut sunucu ile devam et - Enter)
    echo  [2] Yeniden Baslat (Mevcut sureci kapatip burada temiz baslat)
    echo.
    set /p secim="Seciminiz [1/2]: "
    if "%secim%"=="2" (
        echo [KAPATILIYOR] Calisan surec durduruluyor...
        taskkill /f /im llm-yonlendirici.exe >nul 2>&1
        timeout /t 1 >nul
        goto :start_server
    )
    start "" "http://127.0.0.1:8110/ui"
    exit /b 0
)

:start_server

if not exist "llm-yonlendirici.exe" (
    echo [BILGI] Program derleniyor...
    go build -ldflags="-s -w" -o llm-yonlendirici.exe .
    if errorlevel 1 (
        echo [HATA] Derleme basarisiz oldu.
        pause
        exit /b 1
    )
)

echo [BASLATILIYOR] LLM Yonlendirici baslatiliyor...
echo.
echo ====================================================================
echo  Ana Web Hub (Tum Paneller) : http://127.0.0.1:8110/ui
echo  - Zen Kostum Paneli        : http://127.0.0.1:8110/ui
echo  - Proxy Havuzu Paneli      : http://127.0.0.1:8110/ui/proxy
echo  - EVREN Kota Paneli (Hub)  : http://127.0.0.1:8110/ui/evren
echo  - EVREN Dual Web (8088)    : http://127.0.0.1:8088/dashboard
echo ====================================================================
echo.

start "" "http://127.0.0.1:8110/ui"
.\llm-yonlendirici.exe

pause
