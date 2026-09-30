FROM python:3.12-slim

WORKDIR /app

RUN pip install --no-cache-dir fastapi==0.115.0 uvicorn==0.30.6 httpx==0.27.2

COPY proxy.py .

EXPOSE 8000

CMD ["uvicorn", "proxy:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "2", "--log-level", "info"]