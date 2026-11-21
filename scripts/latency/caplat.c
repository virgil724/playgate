// caplat: capture-card latency, measured against DXGI vblank timestamps.
//
// Renders a frame counter as a barcode on the capture card's "monitor" (a
// flip-model swap chain on that output), records the vblank QPC time at which
// each frame was scanned out (IDXGISwapChain::GetFrameStatistics), reads the
// card back through ffmpeg's DirectShow input, decodes the barcode in every
// captured frame and records the QPC time it arrived. Latency(n) =
// arrival(n) - vblank(n), both on QueryPerformanceCounter.
//
// The capture card's HDMI input must be fed by this PC (extended desktop), so
// the card shows up as a monitor. Build (MSYS2 UCRT64):
//   gcc -O2 -o caplat.exe caplat.c -ld3d11 -ldxgi -ldxguid -luser32
// Run from PowerShell (bash eats the backslashes):
//   caplat.exe '\\.\DISPLAY3' 'Live Gamer Ultra-Video' 10 1280 720
// Extra ffmpeg flags: CAPLAT_FFIN (before -f dshow), CAPLAT_FFOUT (after -i).
// To match the host:
//   CAPLAT_FFIN="-fflags nobuffer -flags low_delay -probesize 32 -analyzeduration 0 "
//   CAPLAT_FFOUT="-fps_mode passthrough "
//
// usage: caplat.exe <\\.\DISPLAYn> <dshow video device> [seconds] [capW capH]
#define COBJMACROS
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <d3d11_1.h>
#include <dxgi1_2.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define MAXF 65536
#define NBLK 20 /* [1][0][16 data bits][1][0] */

static LONGLONG vbl[MAXF];   /* frame -> vblank qpc */
static LONGLONG arr[MAXF];   /* frame -> first capture arrival qpc */
static int presFrame[MAXF];  /* present count (mod) -> frame */
static volatile LONG stop;
static int W, H;     /* render (output) size */
static int CW = 1280, CH = 720; /* capture size */
static HANDLE ffOut;
static LONG decoded, garbage, captured;

static int decode(const unsigned char *y) {
  int bits[NBLK];
  for (int i = 0; i < NBLK; i++) {
    int blk = CW / 32, x = i * blk + blk / 2, yy = blk / 2, sum = 0;
    for (int dy = -4; dy <= 4; dy += 4)
      for (int dx = -4; dx <= 4; dx += 4) sum += y[(yy + dy) * CW + x + dx];
    bits[i] = sum / 9 > 128;
  }
  if (!bits[0] || bits[1] || !bits[18] || bits[19]) return -1;
  int n = 0;
  for (int i = 0; i < 16; i++) n = n << 1 | bits[2 + i];
  return n;
}

static DWORD WINAPI reader(LPVOID p) {
  (void)p;
  size_t fsz = (size_t)CW * CH * 3 / 2, got = 0;
  unsigned char *buf = malloc(fsz);
  while (!stop) {
    DWORD n = 0;
    if (!ReadFile(ffOut, buf + got, (DWORD)(fsz - got), &n, NULL) || n == 0) break;
    got += n;
    if (got < fsz) continue;
    got = 0;
    LARGE_INTEGER t;
    QueryPerformanceCounter(&t);
    InterlockedIncrement(&captured);
    int f = decode(buf);
    if (f < 0) { InterlockedIncrement(&garbage); continue; }
    if (f > 0 && f < MAXF && arr[f] == 0) { arr[f] = t.QuadPart; InterlockedIncrement(&decoded); }
  }
  free(buf);
  return 0;
}

static LRESULT CALLBACK wndproc(HWND h, UINT m, WPARAM w, LPARAM l) { return DefWindowProcW(h, m, w, l); }

static int cmpd(const void *a, const void *b) {
  double x = *(const double *)a, y = *(const double *)b;
  return x < y ? -1 : x > y;
}

int main(int argc, char **argv) {
  if (argc < 3) { fprintf(stderr, "usage: caplat <\\\\.\\DISPLAYn> <dshow device> [seconds]\n"); return 2; }
  int secs = argc > 3 ? atoi(argv[3]) : 10;
  if (argc > 5) { CW = atoi(argv[4]); CH = atoi(argv[5]); }
  wchar_t dispW[64];
  MultiByteToWideChar(CP_UTF8, 0, argv[1], -1, dispW, 64);
  SetProcessDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2);

  DEVMODEW dm = {.dmSize = sizeof dm};
  EnumDisplaySettingsW(dispW, ENUM_CURRENT_SETTINGS, &dm);
  printf("display %s: %lux%lu @ %lu Hz\n", argv[1], dm.dmPelsWidth, dm.dmPelsHeight, dm.dmDisplayFrequency);

  IDXGIFactory2 *fac = NULL;
  if (FAILED(CreateDXGIFactory1(&IID_IDXGIFactory2, (void **)&fac))) { puts("no factory"); return 1; }
  IDXGIAdapter1 *ad = NULL, *useAd = NULL;
  RECT rc = {0};
  for (UINT i = 0; !useAd && IDXGIFactory2_EnumAdapters1(fac, i, &ad) == S_OK; i++) {
    IDXGIOutput *out = NULL;
    for (UINT j = 0; IDXGIAdapter1_EnumOutputs(ad, j, &out) == S_OK; j++) {
      DXGI_OUTPUT_DESC od;
      IDXGIOutput_GetDesc(out, &od);
      IDXGIOutput_Release(out);
      if (wcscmp(od.DeviceName, dispW) == 0) { useAd = ad; rc = od.DesktopCoordinates; break; }
    }
    if (!useAd) IDXGIAdapter1_Release(ad);
  }
  if (!useAd) { printf("output %s not found\n", argv[1]); return 1; }
  W = rc.right - rc.left; H = rc.bottom - rc.top;

  ID3D11Device *dev; ID3D11DeviceContext *ctx; ID3D11DeviceContext1 *ctx1;
  if (FAILED(D3D11CreateDevice((IDXGIAdapter *)useAd, D3D_DRIVER_TYPE_UNKNOWN, NULL, 0, NULL, 0, D3D11_SDK_VERSION, &dev, NULL, &ctx))) { puts("no device"); return 1; }
  ID3D11DeviceContext_QueryInterface(ctx, &IID_ID3D11DeviceContext1, (void **)&ctx1);

  WNDCLASSW wc = {.lpfnWndProc = wndproc, .hInstance = GetModuleHandleW(NULL), .lpszClassName = L"caplat"};
  RegisterClassW(&wc);
  HWND hwnd = CreateWindowExW(WS_EX_TOPMOST, L"caplat", L"caplat", WS_POPUP | WS_VISIBLE, rc.left, rc.top, W, H, NULL, NULL, wc.hInstance, NULL);

  DXGI_SWAP_CHAIN_DESC1 sd = {.Width = W, .Height = H, .Format = DXGI_FORMAT_R8G8B8A8_UNORM, .SampleDesc = {1, 0},
                              .BufferUsage = DXGI_USAGE_RENDER_TARGET_OUTPUT, .BufferCount = 2, .SwapEffect = DXGI_SWAP_EFFECT_FLIP_DISCARD};
  IDXGISwapChain1 *sc;
  if (FAILED(IDXGIFactory2_CreateSwapChainForHwnd(fac, (IUnknown *)dev, hwnd, &sd, NULL, NULL, &sc))) { puts("no swapchain"); return 1; }

  // Capture through ffmpeg exactly like the host does (dshow, NV12, raw).
  SECURITY_ATTRIBUTES sa = {sizeof sa, NULL, TRUE};
  HANDLE wr;
  CreatePipe(&ffOut, &wr, &sa, 1 << 22);
  SetHandleInformation(ffOut, HANDLE_FLAG_INHERIT, 0);
  char cmd[1024];
  snprintf(cmd, sizeof cmd,
           "ffmpeg -hide_banner -loglevel error %s-f dshow -rtbufsize 16M -video_size %dx%d -framerate %lu -pixel_format nv12 "
           "-i \"video=%s\" %s-f rawvideo -pix_fmt nv12 pipe:1", getenv("CAPLAT_FFIN") ? getenv("CAPLAT_FFIN") : "", CW, CH, dm.dmDisplayFrequency, argv[2], getenv("CAPLAT_FFOUT") ? getenv("CAPLAT_FFOUT") : "");
  STARTUPINFOA si = {.cb = sizeof si, .dwFlags = STARTF_USESTDHANDLES, .hStdOutput = wr,
                     .hStdError = GetStdHandle(STD_ERROR_HANDLE), .hStdInput = GetStdHandle(STD_INPUT_HANDLE)};
  PROCESS_INFORMATION pi;
  if (!CreateProcessA(NULL, cmd, NULL, NULL, TRUE, 0, NULL, NULL, &si, &pi)) { puts("cannot start ffmpeg"); return 1; }
  CloseHandle(wr);
  HANDLE th = CreateThread(NULL, 0, reader, NULL, 0, NULL);

  float black[4] = {0, 0, 0, 1}, white[4] = {1, 1, 1, 1};
  int total = secs * (int)dm.dmDisplayFrequency, warm = 2 * (int)dm.dmDisplayFrequency;
  if (total + warm >= MAXF) total = MAXF - warm - 1;
  LARGE_INTEGER freq;
  QueryPerformanceFrequency(&freq);
  UINT lastSeen = 0;
  for (int i = 0; i < warm + total; i++) {
    MSG msg;
    while (PeekMessageW(&msg, NULL, 0, 0, PM_REMOVE)) DispatchMessageW(&msg);
    int f = i < warm ? 0 : i - warm + 1; /* frame 0 during warm-up: ffmpeg/dshow starting */
    ID3D11Texture2D *bb; ID3D11RenderTargetView *rtv;
    IDXGISwapChain1_GetBuffer(sc, 0, &IID_ID3D11Texture2D, (void **)&bb);
    ID3D11Device_CreateRenderTargetView(dev, (ID3D11Resource *)bb, NULL, &rtv);
    ID3D11DeviceContext_ClearRenderTargetView(ctx, rtv, black);
    int bits[NBLK] = {1, 0};
    for (int b = 0; b < 16; b++) bits[2 + b] = (f >> (15 - b)) & 1;
    bits[18] = 1; bits[19] = 0;
    D3D11_RECT r[NBLK]; int nr = 0;
    for (int b = 0; b < NBLK; b++)
      if (bits[b]) r[nr++] = (D3D11_RECT){b * (W / 32), 0, (b + 1) * (W / 32), W / 32};
    ID3D11DeviceContext1_ClearView(ctx1, (ID3D11View *)rtv, white, r, nr);
    ID3D11RenderTargetView_Release(rtv);
    ID3D11Texture2D_Release(bb);
    IDXGISwapChain1_Present(sc, 1, 0);
    UINT pc;
    IDXGISwapChain1_GetLastPresentCount(sc, &pc);
    presFrame[pc % MAXF] = f;
    DXGI_FRAME_STATISTICS st;
    if (SUCCEEDED(IDXGISwapChain1_GetFrameStatistics(sc, &st)) && st.PresentCount != lastSeen) {
      lastSeen = st.PresentCount;
      int sf = presFrame[st.PresentCount % MAXF];
      if (sf > 0 && vbl[sf] == 0) vbl[sf] = st.SyncQPCTime.QuadPart;
    }
  }
  Sleep(1500); /* let the last frames arrive */
  InterlockedExchange(&stop, 1);
  TerminateProcess(pi.hProcess, 0);
  WaitForSingleObject(th, 2000);

  static double lat[MAXF];
  int n = 0, onlyV = 0, onlyA = 0;
  for (int f = 1; f <= total; f++) {
    if (vbl[f] && arr[f]) lat[n++] = (double)(arr[f] - vbl[f]) * 1000.0 / freq.QuadPart;
    else if (vbl[f]) onlyV++;
    else if (arr[f]) onlyA++;
  }
  printf("presented %d frames; captured %ld, decoded %ld, garbage %ld; matched %d (vblank only %d, capture only %d)\n",
         total, captured, decoded, garbage, n, onlyV, onlyA);
  if (n == 0) return 1;
  qsort(lat, n, sizeof lat[0], cmpd);
  double sum = 0;
  for (int i = 0; i < n; i++) sum += lat[i];
  printf("latency vblank->arrival ms: min %.1f  p10 %.1f  p50 %.1f  p90 %.1f  p99 %.1f  max %.1f  mean %.1f\n",
         lat[0], lat[n / 10], lat[n / 2], lat[n * 9 / 10], lat[n * 99 / 100], lat[n - 1], sum / n);
  return 0;
}
