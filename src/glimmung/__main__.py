import uvicorn

from glimmung.settings import get_settings


def main() -> None:
    settings = get_settings()
    uvicorn.run(
        "glimmung.app:app",
        host="0.0.0.0",
        port=settings.port,
        log_level=settings.log_level.lower(),
    )


if __name__ == "__main__":
    main()
