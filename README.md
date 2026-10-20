# ninedragons_back

## 환경변수

- `ALLOWED_ORIGINS` — WebSocket 업그레이드와 `/stats`·`/lobby` CORS 에 허용할 브라우저 origin (콤마 구분, 예: `https://ninedragons.gowoobro.com,http://localhost:9003`). 비우면 `https://ninedragons.gowoobro.com`, `http://localhost:9003`, `http://127.0.0.1:9003` 을 쓰고 기동 시 경고를 남긴다. Origin 헤더가 없는 비브라우저 클라이언트는 허용된다.
