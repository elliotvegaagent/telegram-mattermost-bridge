FROM python:3.13-slim AS runtime

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

RUN groupadd --system bridge && useradd --system --gid bridge --home-dir /app bridge

WORKDIR /app
COPY pyproject.toml README.md ./
COPY src ./src
RUN python -m pip install --upgrade pip && python -m pip install . && \
    mkdir -p /data && chown -R bridge:bridge /app /data

USER bridge
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["python", "-m", "bridge"]
CMD ["run"]
