import math


def lorentz_gamma(beta):
    return 1.0 / math.sqrt(1.0 - beta * beta)


def cruise_years(distance_ly, beta):
    return distance_ly / beta


def kinetic_energy_j(mass_kg, beta, speed_of_light_m_s=299792458.0):
    speed = beta * speed_of_light_m_s
    return 0.5 * mass_kg * speed * speed
