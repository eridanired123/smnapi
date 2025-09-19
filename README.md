# API para el SMN de la CONAGUA (Pronóstico a 3 días)

## Descripción

Este programa proporciona una API RESTful para acceder a datos meteorológicos de municipios mexicanos, obtenidos del Servicio Meteorológico Nacional (SMN) de la Comisión Nacional del Agua (CONAGUA). Los datos se actualizan automáticamente cada 3 horas y almacenan en RAM (~15 MB) hasta la próxima actualización, no utiliza caché en disco.

## Instalación y Ejecución

### Prerrequisitos

- Go 1.16 o superior
- arm64 ó amd64

### Ejecución

1. Clona o descarga el código.
2. Ejecuta el siguiente comando en el directorio de smnapi.go:

```bash
go mod init smnapi
go mod tidy
go run smnapi.go
```

3. El servidor estará disponible en `http://localhost:5642`.

#### Con Docker

```bash
docker build -t supersaiyan/smnapi:goku

docker run -d \
	--name=smnapi \
	--restart=unless-stopped \
	supersaiyan/smnapi:goku
```

## Endpoints

### 1. Obtener Pronóstico por ID

```text
GET /forecast/{ides}/{idmun}
```

Devuelve el pronóstico extendido para un municipio específico.

**Parámetros:**

- `ides` (entero): ID del estado

- `idmun` (entero): ID del municipio

**Respuesta:**

```json
{
  "idmun": 1,
  "nmun": "Aguascalientes",
  "ides": 1,
  "nes": "Aguascalientes",
  "last_update": "2023-08-01T12:00:00Z",
  "days": [
    {
      "ndia": 0,
      "dloc": "2023-08-01T12:00:00",
      "tmin": 15.5,
      "tmax": 28.0,
      "probprec": 30,
      "prec": 2.5,
      "desciel": "Parcialmente nublado",
      "velvien": 10.2,
      "dirvieng": 180,
      "dirvienc": "Norte",
      "cc": 40,
      "raf": 17.7
    },
    // ... más días (1-3)
  ]
}
```

### 2. Buscar Municipios

```text
GET /search?mun=nombre&estado=nombre_estado
```

Busca municipios por nombre, con opción de filtrar por estado, admite espacios, busqueda parcial y caracteres especiales.

**Parámetros:**

- `mun` (string, requerido): Nombre o parte del nombre del municipio

- `estado` (string, opcional): Nombre del estado para refinar búsqueda

**Respuesta:**

```json
[
  {
    "idmun": 1,
    "ides": 1,
    "nes": "Aguascalientes",
    "nmun": "Aguascalientes",
    "lat": 3.1416,
    "lon": -3.1416
  },
  // ... más resultados
]
```

### 3. Estado del Servicio

```text
GET /status
```

Devuelve información sobre el estado del servicio y la caché.

**Respuesta:**

```json
{
  "status": "ok",
  "last_update": "2023-08-01T12:00:00Z",
  "count": 2463,
  "now": "2023-08-01T12:05:00Z",
  "index_keys": 8285,
  "index_built": "2023-08-01T12:00:00Z"
}
```

## Características de Búsqueda

- Búsqueda por nombre exacto o parcial

- Normalización automática de texto (sin acentos, minúsculas)

- Búsqueda por combinación municipio|estado

- Límite de 50 resultados por búsqueda

## Configuración

El servicio se ejecuta en el puerto 5642 por defecto. Las principales constantes de configuración se encuentran en el código:

```go
const (
  FUENTE        = "https://smn.conagua.gob.mx/tools/GUI/webservices/index.php?method=1"
  UPDATE_PERIOD = 3 * time.Hour
  HTTP_PORT     = "5642"
  // ...
)
```

## Notas

- Por omisión, los datos se actualizan automáticamente cada 3 horas, siendo configurable. El SMN actualiza su info cada hora y cuarto.

- El servicio incluye CORS habilitado para todos los orígenes.

- Los IDs de municipios y estados son los de CONAGUA.

- El formato de fecha sigue el estándar ISO 8601.

- Los tipos de datos resultantes son:

| NOMBRE   | TIPO    | DESCRIPCIÓN                                                                                                     |
| -------- | ------- | --------------------------------------------------------------------------------------------------------------- |
| cc       | int     | Cobertura de nubes (%)                                                                                          |
| desciel  | string  | Descripción del cielo ("Cielo nublado", "Despejado", "Medio nublado", "Poco nuboso")                            |
| dh       | int     | Diferencia horaria respecto a UTC                                                                               |
| dirvienc | string  | Dirección del viento (Cardinal) ("Este", "Noreste", "Noroeste", "Norte", "Oeste", "Sur", "Sureste", "Suroeste") |
| dirvieng | int     | Dirección del viento (Grados)                                                                                   |
| dloc     | string  | Día local, inicia cuatro horas antes (YYYmmddhhmm)                                                              |
| ides     | int     | ID estado                                                                                                       |
| idmun    | int     | ID municipio                                                                                                    |
| lat      | float64 | Latitud                                                                                                         |
| lon      | float64 | Longitud                                                                                                        |
| ndia     | int     | Número de día (0, 1, 2, 3)                                                                                      |
| nes      | string  | Nombre estado                                                                                                   |
| nmun     | string  | Nombre municipio                                                                                                |
| prec     | float64 | Precipitación (litros/m2)                                                                                       |
| probprec | int     | Probabilidad de precipitación (%)                                                                               |
| raf      | float64 | Ráfagas de viento                                                                                               |
| tmax     | float64 | Temperatura máxima (°C)                                                                                         |
| tmin     | float64 | Temperatura mímima (°C)                                                                                         |
| velvien  | float64 | Velocidad del viento (km/h)                                                                                     |


## Estructura del Código

El código está organizado de la siguiente manera:

- **smnapi.go**: Punto de entrada del programa.

  - **Configuración**: Constantes que definen el comportamiento del servicio.

  - **Estructuras de Datos**: Definiciones de tipos para el pronóstico, municipio, y caché.

  - **Manejo de Caché**: Actualización periódica de datos desde la fuente.

  - **Handlers HTTP**: Manejo de las rutas de la API.

  - **Helpers**: Funciones auxiliares para procesamiento de datos.

## ¿Qué no hace?

- No hay endpoints para búsqueda por latitud y longitud, tal vez algún día.

- No usa los datos del pronóstico por hora (?method=3) porque es un array de 90 MB que me parece una grosería.


## Licencia

GPL3
