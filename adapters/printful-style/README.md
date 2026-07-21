# Printful-style adapter

A stunt adapter for simulating a **Printful-style print-on-demand API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Printful. "Printful" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A behavioral mock of the Printful store + order surface a commerce client uses:

- **Store products:** list, create, and retrieve products
  (`GET`/`POST /v2/store/products`, `GET /v2/store/products/{product_id}`).
- **Orders:** the v1 surface (`POST /orders`, `GET /orders/{order_id}`) — v1
  responses are `result`-wrapped — and the v2 store-order surface
  (`GET`/`POST /v2/store/orders`, `POST /v2/store/orders/{order_id}` to update).
- **Shipping rates:** `POST /v2/shipping/rates` returns synthetic rate quotes.

State persists in SQLite-backed collections, so an order created in one request is
visible when subsequently fetched, within the same `stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/v2/store/products` | `products.star#on_list_products` | List store products |
| POST | `/v2/store/products` | `products.star#on_create_product` | Create a product |
| GET | `/v2/store/products/{product_id}` | `products.star#on_get_product` | Retrieve a product |
| POST | `/orders` | `orders.star#on_create_v1_order` | Create an order (v1, result-wrapped) |
| GET | `/orders/{order_id}` | `orders.star#on_get_v1_order` | Retrieve an order (v1) |
| GET | `/v2/store/orders` | `orders.star#on_list_orders` | List store orders (v2) |
| POST | `/v2/store/orders` | `orders.star#on_create_order` | Create an order (v2) |
| POST | `/v2/store/orders/{order_id}` | `orders.star#on_update_order` | Update an order (v2) |
| POST | `/v2/shipping/rates` | `shipping.star#on_shipping_rates` | Shipping-rate quotes |

Any unmatched route returns `404`.

## Backing stores

| Collection | Purpose |
|------------|---------|
| `products` | Store product records |
| `orders` | Order records (v1 + v2) |

## Auth

Accepts any non-empty **Bearer** key (a dev key — no validation).

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  printful:
    adapter: ./adapters/printful-style
```

Then `stunt up` and point your client at the served address.
