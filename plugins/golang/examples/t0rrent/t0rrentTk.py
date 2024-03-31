#!/usr/bin/python3

import pygame # apt install python3-pygame
from pygame import *
import sys

# Initialize pygame
pygame.init()

# Set up the window
window_width = 920
window_height = 720
screen = pygame.display.set_mode((window_width, window_height))
pygame.display.set_caption('Torrent Visualization')

# Variables to store parsed values
pixelperfect = bool(int((sys.argv[2:2+1]+["0"])[0]))
# pixelperfect = True
square_size = int((sys.argv[1:1+1]+["16"])[0])
torrentname = ""
total_length = 0
piece_length = 0
piece_cnt = 0
redraw = 0
num_columns = 0

def index_startswith(l, s):
    for i in range(len(l)):
        if l[i].startswith(s):
            return i
    raise ValueError("not in list")

def parse_kv(l, s):
    return l[index_startswith(l, s)][len(s):]

def draw_piece(piece_index, lo, hi, fill_color):
    # Calculate the coordinates of the corresponding square
    square_row = piece_index // num_columns
    square_col = piece_index % num_columns
    x_pos = square_col * square_size
    y_pos = square_row * square_size
    x2 = x_pos + square_size
    y2 = y_pos + square_size

    # Calculate the coordinates of the rectangle within the square
    rect_x1 = x_pos + 1
    rect_x2 = x2 - 1
    rect_y1 = y_pos + 1
    rect_y2 = y2 - 1


    psurf = pygame.Surface((square_size, square_size))
    # Draw edge pixels of square

    if not pixelperfect or (lo == 0 and hi == piece_length):
        pygame.draw.rect(psurf, fill_color, (1, 1, square_size - 1, square_size - 1))
    else:
        pygame.draw.rect(psurf, fill_color, (1, 0, square_size - 1, 1))
        pygame.draw.rect(psurf, fill_color, (1, square_size - 1, square_size - 1, 1))
        pygame.draw.rect(psurf, fill_color, (0, 1, 1, square_size - 1))
        pygame.draw.rect(psurf, fill_color, (square_size - 1, 1, square_size, square_size - 1))
        square_inside_sz = square_size - 2
        lo_mapped = int((lo / piece_length) * (square_inside_sz * square_inside_sz))
        hi_mapped = int((hi / piece_length) * (square_inside_sz * square_inside_sz))
        for y in range(1, square_inside_sz + 1):
            rect_start_x = 1
            rect_end_x = square_size -1
            if hi_mapped < rect_start_x or lo_mapped > rect_end_x:
                continue
            if lo_mapped > rect_start_x:
                rect_start_x = lo_mapped
            if hi_mapped < rect_end_x:
                rect_end_x = hi_mapped
            pygame.draw.rect(psurf, fill_color, (rect_start_x, y, rect_end_x - rect_start_x, 1))
    screen.blit(psurf, (x_pos, y_pos))

# Read stdin line by line
quit = False
for line in sys.stdin:
    line = line.rstrip('\n')

    for event in pygame.event.get():
        if (event.type == pygame.QUIT or event.type == KEYDOWN and event.key == K_ESCAPE):
            quit = True
    if quit:
        break


    if redraw > 0:
        piece_cnt = total_length // piece_length
        print("Piece Length =", piece_length)
        print("Total Length =", total_length)
        print("Piece Count  =", piece_cnt)

        # Calculate the number of columns required to fit all squares
        num_columns = int(piece_cnt ** (9/17))
        num_rows = (piece_cnt + num_columns - 1) // num_columns

        # Calculate the required window size to fit all squares
        window_width = num_columns * square_size
        window_height = num_rows * square_size
        screen = pygame.display.set_mode((window_width, window_height))
        pygame.display.update()
        background = pygame.Surface(screen.get_size())

        # Draw squares representing each piece
        x, y = 0, 0
        pygame.draw.rect(background, "black", (x, y, window_width, window_height))
        for i in range(piece_cnt + 1):
            x_pos = x * square_size
            y_pos = y * square_size
            pygame.draw.rect(background, "grey", (x_pos, y_pos, square_size, square_size), 1, 1, 1, 1)
            x += 1
            if x == num_columns:
                x = 0
                y += 1
        screen.blit(background, (0, 0))
        redraw = 0

    if "|ReadAt" in line:
        if piece_length == 0:
            continue

        # Parse variables in the form KEY=value
        piece = line.split()
        piece_index = int(parse_kv(piece, "piece="))
        lo = int(parse_kv(piece, "lo="))
        hi = int(parse_kv(piece, "hi="))
        fill_color = parse_kv(piece, "color=")
        draw_piece(piece_index, lo, hi, fill_color)

    elif "debug: Pieces Length" in line:
        piece_length = int(line.split()[-1])
        if total_length > 0:
            redraw = 1
    elif "debug: Total Length" in line:
        total_length = int(line.split()[-1])
        if piece_length > 0:
            redraw = 1
    elif "debug: File" in line:
        filename = line.split()[-1]
        pygame.display.set_caption(filename)

    pygame.display.update()


pygame.quit()
