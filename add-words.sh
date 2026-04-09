#!/bin/bash
# Usage: ./add-words.sh "word1" "word2" "word3"
# Adds words to techTerms in main.go (used by both Whisper and Deepgram), rebuilds, and relaunches.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
MAIN_FILE="$SCRIPT_DIR/main.go"

if [ $# -eq 0 ]; then
    echo "Usage: $0 \"word1\" \"word2\" ..."
    exit 1
fi

echo "Adding words to STT-Go techTerms:"
for word in "$@"; do
    echo "  + $word"
done

# Add to techTerms slice in main.go (shared by Whisper prompt and Deepgram keyterms)
for word in "$@"; do
    if grep -q "\"$word\"" "$MAIN_FILE"; then
        echo "  [techTerms] '$word' already exists, skipping"
    else
        # Insert after the "Commonly misheard" line
        sed -i "/\/\/ Commonly misheard/a\\\\t\"$word\"," "$MAIN_FILE"
        echo "  [techTerms] added '$word'"
    fi
done

# Convert WSL path to Windows path for PowerShell/cmd
WIN_DIR=$(wslpath -w "$SCRIPT_DIR")

# Kill existing process
echo "Stopping stt-go..."
cmd.exe /c "taskkill /IM stt-go.exe /F" 2>/dev/null || true

# Rebuild — if build fails, still relaunch the old binary
echo "Building..."
if powershell.exe -Command "Set-Location '${WIN_DIR}'; go build -ldflags '-H windowsgui' -o stt-go.exe ."; then
    echo "Build succeeded."
else
    echo "WARNING: Build failed. Relaunching previous binary."
fi

# Always relaunch
echo "Launching..."
cmd.exe /c "start /B ${WIN_DIR}\\stt-go.exe 2>${WIN_DIR}\\stderr.log" 2>&1 &

sleep 2
if [ -s "$SCRIPT_DIR/stderr.log" ]; then
    echo "WARNING: stderr.log is not empty:"
    cat "$SCRIPT_DIR/stderr.log"
else
    echo "Done! STT-Go running with updated words."
fi
