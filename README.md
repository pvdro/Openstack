# OpenStack Epoxy — зеркало upper-constraints (исходники)

Зеркало Python-пакетов из **OpenStack Epoxy (2025.1)** `upper-constraints.txt` в виде source distributions (sdist).

Репозиторий: [pvdro/Openstack](https://github.com/pvdro/Openstack)

## Структура

```
constraints/
  upper-constraints-epoxy.txt   # оригинал (с environment markers)
  requirements-all-epoxy.txt    # уникальные пины без markers
packages/
  epoxy-2025.1/                 # скачанные .tar.gz / .whl (~596 файлов)
scripts/
  download_upper_constraints.go # скачать constraints и запушить в GitHub
```

## Источник constraints

- https://opendev.org/openstack/requirements/raw/branch/stable/2025.1/upper-constraints.txt  
- https://releases.openstack.org/constraints/upper/2025.1  

## Скачать / обновить и сразу залить на GitHub

Нужны: [Go](https://go.dev/dl/), [GitHub CLI](https://cli.github.com/) (`gh auth login`).

```bash
# клонировать репозиторий
git clone https://github.com/pvdro/Openstack.git
cd Openstack

# скачать пакеты Epoxy и запушить в этот же репозиторий
go run ./scripts/download_upper_constraints.go \
  --dest . \
  --packages-subdir packages/epoxy-2025.1 \
  --github-repo pvdro/Openstack \
  --skip-pip
```

Флаги:

| Флаг | Описание |
|------|----------|
| `--github-repo owner/name` | После скачивания сделать commit + push в указанный репозиторий |
| `--github-create` | Создать репозиторий на GitHub, если его ещё нет |
| `--skip-pip` | Только PyPI JSON API (быстрее и надёжнее) |
| `--continue` | Не качать то, что уже есть локально |
| `--commit-message "..."` | Сообщение коммита |
| `--branch main` | Ветка для push |

Без GitHub (только локально):

```bash
go run ./scripts/download_upper_constraints.go \
  --dest ./mirror \
  --skip-pip
```

## Установка пакета из зеркала

```bash
pip install --no-index --find-links=packages/epoxy-2025.1 some-package==x.y.z
```

## Заметки

- Environment markers (`python_version=='3.9'` / `>='3.10'`) сняты: в зеркале обе альтернативные версии.
- Почти всё — sdist (`.tar.gz`). Исключение: `python-linstor===1.24.0` (только wheel на PyPI).
- Зависимости пакетов **не** подтягивались — только строки из upper-constraints.
- Крупные архивы (scipy, pillow, numpy) > 50 MB: GitHub допускает до 100 MB на файл.
